package engine

import (
	"strings"
	"testing"

	"github.com/luthermonson/mayfly/internal/config"
	"github.com/luthermonson/mayfly/internal/state"
)

var testPEM = []byte("-----BEGIN FAKE-----")

func testCfg(pools map[string]config.PoolSpec) *config.Config {
	return &config.Config{
		GitHub:   config.GitHub{Owner: "tricorder", AppID: 1, InstallationID: 2},
		Ephemerd: config.Ephemerd{Version: "v0.1.0"},
		Proxmox:  config.Proxmox{Node: "pve1", Storage: "local-lvm"},
		Defaults: config.Defaults{Cores: 4, MemoryMB: 8192, DiskGB: 60},
		Pools:    pools,
	}
}

func statePool(cfg *config.Config, name string, spec config.PoolSpec, vmids ...int) state.Pool {
	spec = spec.Resolved(cfg.Defaults)
	pool := state.Pool{Name: name, Hostname: spec.Hostname, Token: "tok", Secret: "sec"}
	for _, vmid := range vmids {
		pool.Members = append(pool.Members, state.Member{
			VMID:       vmid,
			Cores:      spec.Cores,
			MemoryMB:   spec.MemoryMB,
			DiskGB:     spec.DiskGB,
			OS:         spec.OS,
			Arch:       spec.Arch,
			Template:   spec.Template,
			ConfigHash: ConfigHash(cfg, nil, spec, &pool, testPEM),
		})
	}
	return pool
}

func TestDiffCreatesPool(t *testing.T) {
	cfg := testCfg(map[string]config.PoolSpec{
		"linux-amd64": {Hostname: "ci-linux-amd64.example.com", Size: 2},
	})
	p := Diff(cfg, &state.Store{}, testPEM, nil, "")

	if len(p.PoolCreates) != 1 || p.PoolCreates[0].Name != "linux-amd64" {
		t.Fatalf("expected one pool create, got %+v", p.PoolCreates)
	}
	if p.PoolCreates[0].Spec.Cores != 4 {
		t.Errorf("defaults not resolved: %+v", p.PoolCreates[0].Spec)
	}
	if len(p.Warnings) != 0 {
		t.Errorf("size>1 pools are fully supported (ephemerd pool mode); unexpected warnings: %v", p.Warnings)
	}
}

func TestDiffNoChanges(t *testing.T) {
	spec := config.PoolSpec{Hostname: "ci.example.com", Size: 1}
	cfg := testCfg(map[string]config.PoolSpec{"p": spec})
	st := &state.Store{Pools: []state.Pool{statePool(cfg, "p", spec, 101)}}

	p := Diff(cfg, st, testPEM, nil, "")
	if !p.Empty() {
		var b strings.Builder
		p.Render(&b)
		t.Errorf("expected empty plan, got:\n%s", b.String())
	}
}

func TestDiffConfigChangeIsUpdateNotReplace(t *testing.T) {
	spec := config.PoolSpec{Hostname: "ci.example.com", Size: 1}
	cfg := testCfg(map[string]config.PoolSpec{"p": spec})
	st := &state.Store{Pools: []state.Pool{statePool(cfg, "p", spec, 101)}}

	// Adding a friend's repo: config tier only.
	cfg.Pools["p"] = config.PoolSpec{Hostname: "ci.example.com", Size: 1, Repos: []string{"friend/project"}}
	p := Diff(cfg, st, testPEM, nil, "")

	if len(p.Replacements) != 0 {
		t.Fatalf("repo change must not replace: %+v", p.Replacements)
	}
	if len(p.Updates) != 1 || len(p.Updates[0].Members) != 1 {
		t.Fatalf("expected one member update, got %+v", p.Updates)
	}
	mu := p.Updates[0].Members[0]
	if !mu.Config || mu.PowerCycle || mu.DiskGrowGB != 0 {
		t.Errorf("expected config-only tier, got %+v", mu)
	}
}

func TestDiffCoresChangeIsPowerCycle(t *testing.T) {
	spec := config.PoolSpec{Hostname: "ci.example.com", Size: 1, Cores: 4}
	cfg := testCfg(map[string]config.PoolSpec{"p": spec})
	st := &state.Store{Pools: []state.Pool{statePool(cfg, "p", spec, 101)}}

	cfg.Pools["p"] = config.PoolSpec{Hostname: "ci.example.com", Size: 1, Cores: 8}
	p := Diff(cfg, st, testPEM, nil, "")

	if len(p.Replacements) != 0 {
		t.Fatalf("cores change must not replace: %+v", p.Replacements)
	}
	if len(p.Updates) != 1 {
		t.Fatalf("expected update, got %+v", p.Updates)
	}
	mu := p.Updates[0].Members[0]
	if !mu.PowerCycle || mu.Config || mu.DiskGrowGB != 0 {
		t.Errorf("expected power-cycle tier only, got %+v", mu)
	}
	if mu.FromCores != 4 {
		t.Errorf("expected FromCores 4, got %d", mu.FromCores)
	}
}

func TestDiffDiskGrowIsLive(t *testing.T) {
	spec := config.PoolSpec{Hostname: "ci.example.com", Size: 1, DiskGB: 60}
	cfg := testCfg(map[string]config.PoolSpec{"p": spec})
	st := &state.Store{Pools: []state.Pool{statePool(cfg, "p", spec, 101)}}

	cfg.Pools["p"] = config.PoolSpec{Hostname: "ci.example.com", Size: 1, DiskGB: 120}
	p := Diff(cfg, st, testPEM, nil, "")

	if len(p.Replacements) != 0 {
		t.Fatalf("disk grow must not replace: %+v", p.Replacements)
	}
	mu := p.Updates[0].Members[0]
	if mu.DiskGrowGB != 120 || mu.Config || mu.PowerCycle {
		t.Errorf("expected disk-grow tier only, got %+v", mu)
	}
}

func TestDiffDiskShrinkReplaces(t *testing.T) {
	spec := config.PoolSpec{Hostname: "ci.example.com", Size: 1, DiskGB: 120}
	cfg := testCfg(map[string]config.PoolSpec{"p": spec})
	st := &state.Store{Pools: []state.Pool{statePool(cfg, "p", spec, 101)}}

	cfg.Pools["p"] = config.PoolSpec{Hostname: "ci.example.com", Size: 1, DiskGB: 60}
	p := Diff(cfg, st, testPEM, nil, "")

	if len(p.Replacements) != 1 || len(p.Replacements[0].VMIDs) != 1 {
		t.Fatalf("disk shrink should replace, got %+v", p.Replacements)
	}
	if len(p.Updates) != 0 {
		t.Errorf("no in-place work expected, got %+v", p.Updates)
	}
}

func TestDiffTemplateChangeReplaces(t *testing.T) {
	spec := config.PoolSpec{Hostname: "ci.example.com", Size: 1}
	cfg := testCfg(map[string]config.PoolSpec{"p": spec})
	st := &state.Store{Pools: []state.Pool{statePool(cfg, "p", spec, 101)}}

	cfg.Pools["p"] = config.PoolSpec{Hostname: "ci.example.com", Size: 1, Template: "mayfly-ubuntu-26.04-amd64"}
	p := Diff(cfg, st, testPEM, nil, "")

	if len(p.Replacements) != 1 {
		t.Fatalf("template change should replace, got %+v", p.Replacements)
	}
}

func TestDiffArchChangeReplaces(t *testing.T) {
	spec := config.PoolSpec{Hostname: "ci.example.com", Size: 1}
	cfg := testCfg(map[string]config.PoolSpec{"p": spec})
	st := &state.Store{Pools: []state.Pool{statePool(cfg, "p", spec, 101)}}

	cfg.Pools["p"] = config.PoolSpec{Hostname: "ci.example.com", Size: 1, Arch: "arm64"}
	p := Diff(cfg, st, testPEM, nil, "")

	if len(p.Replacements) != 1 {
		t.Fatalf("arch change should replace, got %+v", p.Replacements)
	}
}

// The core fail-open fix: turning on vlan_tag for a pool whose members were
// built untagged must plan a REPLACE (the tag is a NIC property set at clone
// time), not silently produce "No changes" and leave runners on the flat LAN.
func TestDiffVLANTagChangeReplaces(t *testing.T) {
	spec := config.PoolSpec{Hostname: "ci.example.com", Size: 1}
	cfg := testCfg(map[string]config.PoolSpec{"p": spec})
	// Member recorded untagged (statePool leaves VLANTag/Firewall zero).
	st := &state.Store{Pools: []state.Pool{statePool(cfg, "p", spec, 101)}}

	// Operator rolls the pool onto an isolated VLAN.
	cfg.Pools["p"] = config.PoolSpec{Hostname: "ci.example.com", Size: 1, VLANTag: 42}
	p := Diff(cfg, st, testPEM, nil, "")

	if len(p.Replacements) != 1 || len(p.Replacements[0].VMIDs) != 1 || p.Replacements[0].VMIDs[0] != 101 {
		t.Fatalf("vlan_tag change must replace member 101, got %+v", p.Replacements)
	}
	if len(p.Updates) != 0 {
		t.Errorf("a replace subsumes in-place work; expected no updates, got %+v", p.Updates)
	}
	if p.Empty() {
		t.Fatal("plan must not be empty — the whole point is the change is now visible in `mayfly plan`")
	}
}

// Changing the firewall flag on an existing member is likewise a NIC-property
// change and must force a replace, not pass as no-op.
func TestDiffFirewallChangeReplaces(t *testing.T) {
	spec := config.PoolSpec{Hostname: "ci.example.com", Size: 1}
	cfg := testCfg(map[string]config.PoolSpec{"p": spec})
	st := &state.Store{Pools: []state.Pool{statePool(cfg, "p", spec, 101)}}

	cfg.Pools["p"] = config.PoolSpec{Hostname: "ci.example.com", Size: 1, Firewall: true}
	p := Diff(cfg, st, testPEM, nil, "")

	if len(p.Replacements) != 1 || len(p.Replacements[0].VMIDs) != 1 {
		t.Fatalf("firewall change must replace, got %+v", p.Replacements)
	}
}

// Setting vm_storage on a pool whose members were built as linked clones
// (recorded VMStorage empty) must plan a REPLACE — vm_storage is a clone-time
// disk-placement property, so an in-place update can't move the existing disk.
// Without this the feature would only ever benefit newly-scaled members and
// leave existing runners trapped on the template's storage.
func TestDiffVMStorageChangeReplaces(t *testing.T) {
	spec := config.PoolSpec{Hostname: "ci.example.com", Size: 1}
	cfg := testCfg(map[string]config.PoolSpec{"p": spec})
	// Member recorded as a linked clone (statePool leaves VMStorage empty).
	st := &state.Store{Pools: []state.Pool{statePool(cfg, "p", spec, 101)}}

	// Operator moves the pool's disk onto a separate storage (full clone).
	cfg.Pools["p"] = config.PoolSpec{Hostname: "ci.example.com", Size: 1, VMStorage: "local-lvm"}
	p := Diff(cfg, st, testPEM, nil, "")

	if len(p.Replacements) != 1 || len(p.Replacements[0].VMIDs) != 1 || p.Replacements[0].VMIDs[0] != 101 {
		t.Fatalf("vm_storage change must replace member 101, got %+v", p.Replacements)
	}
	if len(p.Updates) != 0 {
		t.Errorf("a replace subsumes in-place work; expected no updates, got %+v", p.Updates)
	}
	if p.Empty() {
		t.Fatal("plan must not be empty — a vm_storage change must be visible in `mayfly plan`")
	}
}

// A member already built on the pool's vm_storage must not drift: the recorded
// VMStorage round-trips and compares equal to the desired spec, so no replace.
func TestDiffVMStorageInSyncNoChange(t *testing.T) {
	spec := config.PoolSpec{Hostname: "ci.example.com", Size: 1, VMStorage: "local-lvm"}
	cfg := testCfg(map[string]config.PoolSpec{"p": spec})
	pool := statePool(cfg, "p", spec, 101)
	// statePool doesn't set VMStorage; mirror what provisionMember records.
	pool.Members[0].VMStorage = "local-lvm"
	st := &state.Store{Pools: []state.Pool{pool}}

	p := Diff(cfg, st, testPEM, nil, "")
	if len(p.Replacements) != 0 {
		t.Fatalf("member already on vm_storage local-lvm must not replace, got %+v", p.Replacements)
	}
}

// A member already built with the pool's tag/firewall must not drift: the
// recorded VLANTag/Firewall round-trip and compare equal to the desired spec.
func TestDiffVLANTagInSyncNoChange(t *testing.T) {
	spec := config.PoolSpec{Hostname: "ci.example.com", Size: 1, VLANTag: 42, Firewall: true}
	cfg := testCfg(map[string]config.PoolSpec{"p": spec})
	pool := statePool(cfg, "p", spec, 101)
	// statePool doesn't set NIC fields; mirror what provisionMember records.
	pool.Members[0].VLANTag = 42
	pool.Members[0].Firewall = true
	st := &state.Store{Pools: []state.Pool{pool}}

	p := Diff(cfg, st, testPEM, nil, "")
	if len(p.Replacements) != 0 {
		t.Fatalf("member already on tag 42 + firewall must not replace, got %+v", p.Replacements)
	}
	if len(p.Updates) != 0 {
		t.Errorf("no in-place work expected, got %+v", p.Updates)
	}
}

// firewall=true must produce a loud plan-time warning: on its own it writes
// firewall=1 on the vNIC but enforces nothing without the datacenter/VM
// firewall, so the operator must not be falsely reassured.
func TestDiffFirewallEmitsWarning(t *testing.T) {
	cfg := testCfg(map[string]config.PoolSpec{
		"p": {Hostname: "ci.example.com", Size: 1, Firewall: true},
	})
	p := Diff(cfg, &state.Store{}, testPEM, nil, "")

	found := false
	for _, w := range p.Warnings {
		if strings.Contains(w, "firewall=true") && strings.Contains(w, "enforces NOTHING") {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected a loud firewall enforcement warning, got %v", p.Warnings)
	}

	// A pool that leaves firewall off gets no such warning.
	cfg2 := testCfg(map[string]config.PoolSpec{"p": {Hostname: "ci.example.com", Size: 1}})
	p2 := Diff(cfg2, &state.Store{}, testPEM, nil, "")
	for _, w := range p2.Warnings {
		if strings.Contains(w, "firewall") {
			t.Errorf("firewall-off pool must not warn about the firewall, got %q", w)
		}
	}
}

// vlanBridgeError is the preflight decision: a tagged pool on a non-VLAN-aware
// bridge is the silent fail-open (tag written, VM stays on the flat LAN) and
// must error; every other combination must pass. This table-tests the branch
// directly so the "vlan-aware vs not" logic is pinned without a live Proxmox.
func TestVLANBridgeError(t *testing.T) {
	tests := []struct {
		name      string
		vlanTag   int
		vlanAware bool
		wantErr   bool
	}{
		{"tagged on non-vlan-aware bridge fails", 42, false, true},
		{"tagged on vlan-aware bridge passes", 42, true, false},
		{"untagged on non-vlan-aware bridge passes", 0, false, false},
		{"untagged on vlan-aware bridge passes", 0, true, false},
		{"negative tag treated as untagged", -1, false, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := vlanBridgeError("p", tt.vlanTag, "node-a", "vmbr0", tt.vlanAware)
			if (err != nil) != tt.wantErr {
				t.Fatalf("vlanBridgeError(tag=%d, aware=%v) err=%v, wantErr=%v",
					tt.vlanTag, tt.vlanAware, err, tt.wantErr)
			}
			if !tt.wantErr {
				return
			}
			// The error must be actionable: name the pool, bridge, node, tag,
			// and how to fix it, so the operator isn't left guessing.
			for _, want := range []string{"p", "vlan_tag=42", "vmbr0", "node-a", "bridge-vlan-aware yes"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q missing %q", err.Error(), want)
				}
			}
		})
	}
}

func TestDiffWindowsPoolCreate(t *testing.T) {
	cfg := testCfg(map[string]config.PoolSpec{
		"win": {Hostname: "ci-win.example.com", Size: 1, OS: "windows", Template: "mayfly-winserver-2025"},
	})
	p := Diff(cfg, &state.Store{}, testPEM, nil, "")

	if len(p.PoolCreates) != 1 {
		t.Fatalf("expected windows pool create, got %+v", p.PoolCreates)
	}
	spec := p.PoolCreates[0].Spec
	if spec.OS != "windows" || spec.Arch != "amd64" || spec.Template != "mayfly-winserver-2025" {
		t.Errorf("windows spec not resolved: %+v", spec)
	}
}

func TestDiffDarwinEnrollUnenrollAndDrift(t *testing.T) {
	spec := config.PoolSpec{Hostname: "ci-mac.example.com", OS: "darwin", Members: []string{"mini1.lan", "mini2.lan"}, SSHUser: "luther"}
	cfg := testCfg(map[string]config.PoolSpec{"mac": spec})

	// New pool → create with hosts.
	p := Diff(cfg, &state.Store{}, testPEM, nil, "")
	if len(p.PoolCreates) != 1 || p.PoolCreates[0].Spec.OS != "darwin" {
		t.Fatalf("expected darwin pool create, got %+v", p.PoolCreates)
	}

	// mini1 enrolled + in sync, mini3 enrolled but no longer declared,
	// mini2 declared but not enrolled.
	resolved := spec.Resolved(cfg.Defaults)
	pool := state.Pool{Name: "mac", Hostname: resolved.Hostname, Token: "tok", Secret: "sec", SSHUser: "luther"}
	hash := ConfigHash(cfg, nil, resolved, &pool, testPEM)
	pool.Members = []state.Member{
		{Hostname: "mini1.lan", OS: "darwin", Arch: "arm64", ConfigHash: hash},
		{Hostname: "mini3.lan", OS: "darwin", Arch: "arm64", ConfigHash: hash},
	}
	st := &state.Store{Pools: []state.Pool{pool}}

	p2 := Diff(cfg, st, testPEM, nil, "")
	if len(p2.ScaleUps) != 1 || len(p2.ScaleUps[0].Hosts) != 1 || p2.ScaleUps[0].Hosts[0] != "mini2.lan" {
		t.Errorf("expected enroll of mini2.lan, got %+v", p2.ScaleUps)
	}
	if len(p2.ScaleDowns) != 1 || len(p2.ScaleDowns[0].Hosts) != 1 || p2.ScaleDowns[0].Hosts[0] != "mini3.lan" {
		t.Errorf("expected unenroll of mini3.lan, got %+v", p2.ScaleDowns)
	}
	if len(p2.Replacements) != 0 {
		t.Errorf("darwin pools must never plan replacements: %+v", p2.Replacements)
	}
	if len(p2.Updates) != 0 {
		t.Errorf("in-sync member should not update: %+v", p2.Updates)
	}

	// Config drift (add a repo) → config-tier update on enrolled members only.
	cfg.Pools["mac"] = config.PoolSpec{Hostname: "ci-mac.example.com", OS: "darwin",
		Members: []string{"mini1.lan"}, SSHUser: "luther", Repos: []string{"friend/project"}}
	st2 := &state.Store{Pools: []state.Pool{{
		Name: "mac", Hostname: "ci-mac.example.com", Token: "tok", Secret: "sec", SSHUser: "luther",
		Members: []state.Member{{Hostname: "mini1.lan", OS: "darwin", Arch: "arm64", ConfigHash: hash}},
	}}}
	p3 := Diff(cfg, st2, testPEM, nil, "")
	if len(p3.Updates) != 1 || len(p3.Updates[0].Members) != 1 ||
		p3.Updates[0].Members[0].Host != "mini1.lan" || !p3.Updates[0].Members[0].Config {
		t.Errorf("expected config-tier ssh update for mini1.lan, got %+v", p3.Updates)
	}
}

func TestEnrollServiceStart(t *testing.T) {
	linux := enrollServiceStart("linux")
	if !strings.Contains(linux, "systemctl enable ephemerd") ||
		!strings.Contains(linux, "systemctl restart ephemerd") ||
		!strings.Contains(linux, "systemctl daemon-reload") {
		t.Errorf("linux service start must drive systemd, got:\n%s", linux)
	}
	if strings.Contains(linux, "systemctl") == false {
		t.Errorf("expected systemctl for linux")
	}

	darwin := enrollServiceStart("darwin")
	if strings.Contains(darwin, "systemctl") {
		t.Errorf("darwin must not use systemctl, got:\n%s", darwin)
	}
	if !strings.Contains(darwin, "ephemerd restart") || !strings.Contains(darwin, "ephemerd start") {
		t.Errorf("darwin service start must use ephemerd restart/start, got:\n%s", darwin)
	}
}

func TestDiffLinuxEnrollUsesSSHNotVMs(t *testing.T) {
	spec := config.PoolSpec{
		Hostname: "ci-arm.example.com", OS: "linux", Arch: "arm64",
		Members: []string{"arm1.lan", "arm2.lan"}, SSHUser: "luther",
	}
	cfg := testCfg(map[string]config.PoolSpec{"arm": spec})

	// New linux-enroll pool → a pool create carrying the resolved enroll spec.
	p := Diff(cfg, &state.Store{}, testPEM, nil, "")
	if len(p.PoolCreates) != 1 {
		t.Fatalf("expected one pool create, got %+v", p.PoolCreates)
	}
	got := p.PoolCreates[0].Spec
	if !got.UsesEnroll() || got.OS != "linux" || got.Size != 2 || got.Template != "" {
		t.Fatalf("linux enroll spec not resolved for SSH path: %+v", got)
	}

	// arm1 enrolled + in sync, arm3 enrolled but no longer declared, arm2
	// declared but not enrolled. Enrolled members carry no VMID.
	resolved := spec.Resolved(cfg.Defaults)
	pool := state.Pool{Name: "arm", Hostname: resolved.Hostname, Token: "tok", Secret: "sec", SSHUser: "luther"}
	hash := ConfigHash(cfg, nil, resolved, &pool, testPEM)
	pool.Members = []state.Member{
		{Hostname: "arm1.lan", OS: "linux", Arch: "arm64", ConfigHash: hash},
		{Hostname: "arm3.lan", OS: "linux", Arch: "arm64", ConfigHash: hash},
	}
	st := &state.Store{Pools: []state.Pool{pool}}

	p2 := Diff(cfg, st, testPEM, nil, "")
	if len(p2.ScaleUps) != 1 || len(p2.ScaleUps[0].Hosts) != 1 || p2.ScaleUps[0].Hosts[0] != "arm2.lan" {
		t.Errorf("expected SSH enroll of arm2.lan, got %+v", p2.ScaleUps)
	}
	if len(p2.ScaleDowns) != 1 || len(p2.ScaleDowns[0].Hosts) != 1 || p2.ScaleDowns[0].Hosts[0] != "arm3.lan" {
		t.Errorf("expected SSH unenroll of arm3.lan, got %+v", p2.ScaleDowns)
	}
	if len(p2.Replacements) != 0 {
		t.Errorf("enrolled linux pools must never plan replacements: %+v", p2.Replacements)
	}
	if len(p2.TemplateCreates) != 0 {
		t.Errorf("enrolled linux pools need no template: %+v", p2.TemplateCreates)
	}
}

func TestDiffMissingTemplatesFlowThrough(t *testing.T) {
	cfg := testCfg(nil)
	missing := []TemplateCreate{{Name: "mayfly-ubuntu-24.04-arm64", Arch: "arm64", Node: "pve-arm", Storage: "local-lvm"}}
	p := Diff(cfg, &state.Store{}, testPEM, missing, "")
	if len(p.TemplateCreates) != 1 || p.TemplateCreates[0].Arch != "arm64" {
		t.Errorf("missing templates should appear in plan: %+v", p.TemplateCreates)
	}
	if p.Empty() {
		t.Error("plan with template creates must not be empty")
	}
}

func TestDiffForceReplace(t *testing.T) {
	spec := config.PoolSpec{Hostname: "ci.example.com", Size: 1}
	cfg := testCfg(map[string]config.PoolSpec{"p": spec})
	st := &state.Store{Pools: []state.Pool{statePool(cfg, "p", spec, 101)}}

	p := Diff(cfg, st, testPEM, nil, "p")
	if len(p.Replacements) != 1 || len(p.Replacements[0].VMIDs) != 1 {
		t.Fatalf("--replace should force rebuild, got %+v", p.Replacements)
	}
}

func TestDiffHostnameChangeIsDNSSwapPlusConfig(t *testing.T) {
	spec := config.PoolSpec{Hostname: "old.example.com", Size: 1}
	cfg := testCfg(map[string]config.PoolSpec{"p": spec})
	st := &state.Store{Pools: []state.Pool{statePool(cfg, "p", spec, 101)}}

	cfg.Pools["p"] = config.PoolSpec{Hostname: "new.example.com", Size: 1}
	p := Diff(cfg, st, testPEM, nil, "")

	if len(p.Replacements) != 0 {
		t.Fatalf("hostname change must not replace: %+v", p.Replacements)
	}
	if len(p.Updates) != 1 || p.Updates[0].NewHostname != "new.example.com" {
		t.Fatalf("expected DNS swap update, got %+v", p.Updates)
	}
	if len(p.Updates[0].Members) != 1 || !p.Updates[0].Members[0].Config {
		t.Errorf("hostname change should re-render config on members: %+v", p.Updates[0].Members)
	}
}

func TestDiffScaleUpDown(t *testing.T) {
	spec := config.PoolSpec{Hostname: "ci.example.com", Size: 3}
	cfg := testCfg(map[string]config.PoolSpec{"p": spec})
	st := &state.Store{Pools: []state.Pool{statePool(cfg, "p", spec, 101)}}

	p := Diff(cfg, st, testPEM, nil, "")
	if len(p.ScaleUps) != 1 || p.ScaleUps[0].Count != 2 {
		t.Errorf("expected scale up by 2, got %+v", p.ScaleUps)
	}

	cfg.Pools["p"] = config.PoolSpec{Hostname: "ci.example.com", Size: 0}
	st2 := &state.Store{Pools: []state.Pool{statePool(cfg, "p", cfg.Pools["p"], 101, 102)}}
	p2 := Diff(cfg, st2, testPEM, nil, "")
	if len(p2.ScaleDowns) != 1 || len(p2.ScaleDowns[0].VMIDs) != 2 {
		t.Errorf("expected scale down of 2, got %+v", p2.ScaleDowns)
	}
}

func TestDiffDestroysRemovedPool(t *testing.T) {
	cfg := testCfg(map[string]config.PoolSpec{})
	st := &state.Store{Pools: []state.Pool{{Name: "old-pool"}}}

	p := Diff(cfg, st, testPEM, nil, "")
	if len(p.PoolDestroys) != 1 || p.PoolDestroys[0] != "old-pool" {
		t.Errorf("expected old-pool destroy, got %v", p.PoolDestroys)
	}
}
