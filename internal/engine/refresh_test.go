package engine

import (
	"errors"
	"strings"
	"testing"

	"github.com/ephemerd/mayfly/internal/config"
	"github.com/ephemerd/mayfly/internal/state"
)

func TestReconcileVM(t *testing.T) {
	member := state.Member{VMID: 101, PVENode: "pve1", Cores: 4, MemoryMB: 8192}

	tests := []struct {
		name       string
		liveExists bool
		liveCores  int
		liveMemMB  int
		wantKind   string
		wantRemove bool
		wantCores  int
		wantMemMB  int
	}{
		{name: "gone", liveExists: false, wantKind: "vm-missing", wantRemove: true},
		{name: "in sync", liveExists: true, liveCores: 4, liveMemMB: 8192, wantKind: ""},
		{name: "cores drift", liveExists: true, liveCores: 2, liveMemMB: 8192,
			wantKind: "vm-resources", wantCores: 2, wantMemMB: 8192},
		{name: "memory drift", liveExists: true, liveCores: 4, liveMemMB: 4096,
			wantKind: "vm-resources", wantCores: 4, wantMemMB: 4096},
		{name: "both drift", liveExists: true, liveCores: 8, liveMemMB: 16384,
			wantKind: "vm-resources", wantCores: 8, wantMemMB: 16384},
		// Unpopulated list fields must not be read as drift-to-zero.
		{name: "zero live fields ignored", liveExists: true, liveCores: 0, liveMemMB: 0, wantKind: ""},
		{name: "zero cores only", liveExists: true, liveCores: 0, liveMemMB: 4096,
			wantKind: "vm-resources", wantCores: 4, wantMemMB: 4096},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			act := reconcileVM(member, tc.liveExists, tc.liveCores, tc.liveMemMB)
			if act.Kind != tc.wantKind {
				t.Fatalf("kind = %q, want %q", act.Kind, tc.wantKind)
			}
			if act.Remove != tc.wantRemove {
				t.Errorf("remove = %v, want %v", act.Remove, tc.wantRemove)
			}
			if tc.wantKind == "vm-resources" && (act.Cores != tc.wantCores || act.MemMB != tc.wantMemMB) {
				t.Errorf("adopted cores=%d mem=%d, want cores=%d mem=%d",
					act.Cores, act.MemMB, tc.wantCores, tc.wantMemMB)
			}
		})
	}
}

func TestReconcileDarwin(t *testing.T) {
	tests := []struct {
		name      string
		probeErr  error
		output    string
		wantKind  string
		wantClear bool
	}{
		{name: "unreachable", probeErr: errors.New("ssh: connect refused"), wantKind: "ssh-unreachable", wantClear: false},
		{name: "ephemerd gone", output: "MISSING\n", wantKind: "ephemerd-missing", wantClear: true},
		{name: "healthy", output: "ephemerd running, 0 jobs\n", wantKind: ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			act := reconcileDarwin(tc.probeErr, tc.output)
			if act.Kind != tc.wantKind {
				t.Fatalf("kind = %q, want %q", act.Kind, tc.wantKind)
			}
			if act.ClearConfig != tc.wantClear {
				t.Errorf("clearConfig = %v, want %v", act.ClearConfig, tc.wantClear)
			}
		})
	}
}

func TestReconcilePoolVMs(t *testing.T) {
	pool := state.Pool{
		Name: "p",
		Members: []state.Member{
			{VMID: 101, PVENode: "pve1", Cores: 4, MemoryMB: 8192},
			{VMID: 102, PVENode: "pve1", Cores: 4, MemoryMB: 8192},
			{Hostname: "mini1.lan", OS: "darwin", ConfigHash: "abc"}, // must be untouched
		},
	}
	live := map[string]map[int]liveVM{
		"pve1": {
			// 101 deleted out-of-band; 102 hand-tweaked to 2 cores.
			102: {Cores: 2, MemMB: 8192},
		},
	}

	findings := reconcilePoolVMs(&pool, live)
	if len(findings) != 2 {
		t.Fatalf("expected 2 findings, got %+v", findings)
	}
	if findings[0].Kind != "vm-missing" || findings[0].Member != "101" {
		t.Errorf("finding[0] = %+v, want vm-missing for 101", findings[0])
	}
	if findings[1].Kind != "vm-resources" || findings[1].Member != "102" {
		t.Errorf("finding[1] = %+v, want vm-resources for 102", findings[1])
	}

	if len(pool.Members) != 2 {
		t.Fatalf("member 101 should be removed, got %+v", pool.Members)
	}
	if m, ok := pool.FindMember(102); !ok || m.Cores != 2 || m.MemoryMB != 8192 {
		t.Errorf("member 102 should record live cores=2, got %+v", m)
	}
	if m, ok := pool.FindMemberByHost("mini1.lan"); !ok || m.ConfigHash != "abc" {
		t.Errorf("darwin member must not be touched by VM reconciliation: %+v", pool.Members)
	}
}

// After refresh mutates state, Diff must plan the corrective actions: a
// scale-up for the missing VM and a power-cycle for the drifted one.
func TestRefreshMutationsFeedDiff(t *testing.T) {
	spec := config.PoolSpec{Hostname: "ci.example.com", Size: 2}
	cfg := testCfg(map[string]config.PoolSpec{"p": spec})
	pool := statePool(cfg, "p", spec, 101, 102)
	for i := range pool.Members {
		pool.Members[i].PVENode = "pve1"
	}
	st := &state.Store{Pools: []state.Pool{pool}}

	live := map[string]map[int]liveVM{
		"pve1": {102: {Cores: 2, MemMB: 8192}},
	}
	reconcilePoolVMs(&st.Pools[0], live)

	p := Diff(cfg, st, testPEM, nil, "")
	if len(p.ScaleUps) != 1 || p.ScaleUps[0].Count != 1 {
		t.Errorf("expected scale-up of 1 to replace missing VM, got %+v", p.ScaleUps)
	}
	if len(p.Updates) != 1 || len(p.Updates[0].Members) != 1 {
		t.Fatalf("expected one member update, got %+v", p.Updates)
	}
	mu := p.Updates[0].Members[0]
	if mu.VMID != 102 || !mu.PowerCycle || mu.FromCores != 2 {
		t.Errorf("expected power-cycle for 102 from live cores=2, got %+v", mu)
	}
	if len(p.Replacements) != 0 {
		t.Errorf("refresh drift must not cause replacements: %+v", p.Replacements)
	}
}

func TestRefreshResultRender(t *testing.T) {
	rr := &RefreshResult{Findings: []DriftFinding{
		{Pool: "mac-arm64", Member: "192.0.2.78", Kind: "ephemerd-missing", Detail: "ephemerd missing (will re-enroll)"},
		{Pool: "linux", Kind: "tunnel-missing", Detail: "tunnel abc deleted"},
	}}
	var b strings.Builder
	rr.Render(&b)
	out := b.String()
	if !strings.Contains(out, "  ! drift pool.mac-arm64 192.0.2.78: ephemerd missing (will re-enroll)") {
		t.Errorf("member finding misrendered:\n%s", out)
	}
	if !strings.Contains(out, "  ! drift pool.linux: tunnel abc deleted") {
		t.Errorf("pool-level finding misrendered:\n%s", out)
	}

	var empty strings.Builder
	(&RefreshResult{}).Render(&empty)
	if empty.Len() != 0 {
		t.Errorf("no findings should render nothing, got %q", empty.String())
	}
}
