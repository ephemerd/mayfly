package engine

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/luthermonson/mayfly/internal/config"
	"github.com/luthermonson/mayfly/internal/provider"
	"github.com/luthermonson/mayfly/internal/state"
)

// recordProvider records the nodes EnsureTemplate was called for, and inherits
// fakeProvider's controllable NodeReachable (via its down set).
type recordProvider struct {
	fakeProvider
	ensured *[]string
}

func (r recordProvider) EnsureTemplate(_ context.Context, spec provider.TemplateSpec) error {
	*r.ensured = append(*r.ensured, spec.Node)
	return nil
}

// A template-create on an unreachable node must be skipped with a warning while
// a template on a reachable node in the SAME apply still gets built. This is the
// whole point of the fix: one dead node (node-a returning a truncated API body)
// must not abort reconciliation for the rest of the fleet.
func TestApplySkipsTemplateCreateOnUnreachableNode(t *testing.T) {
	var ensured []string
	rec := recordProvider{
		fakeProvider: fakeProvider{down: map[string]bool{"node-a": true}},
		ensured:      &ensured,
	}
	out := &bytes.Buffer{}
	e := &Engine{
		Cfg:       &config.Config{Proxmox: config.Proxmox{Node: "node-b"}},
		Out:       out,
		provCache: map[config.Provider]provider.Provider{config.ProviderProxmox: rec},
		St:        &state.Store{},
	}
	p := &Plan{TemplateCreates: []TemplateCreate{
		{Name: "mayfly-ubuntu-24.04-amd64", Arch: "amd64", Node: "node-a", Storage: "local", Provider: config.ProviderProxmox},
		{Name: "mayfly-ubuntu-24.04-amd64", Arch: "amd64", Node: "node-b", Storage: "local", Provider: config.ProviderProxmox},
	}}
	if err := e.apply(context.Background(), p); err != nil {
		t.Fatalf("apply aborted on an unreachable node, want fail-soft: %v", err)
	}
	if len(ensured) != 1 || ensured[0] != "node-b" {
		t.Fatalf("EnsureTemplate calls = %v, want exactly [node-b] (node-a skipped)", ensured)
	}
	if s := out.String(); !strings.Contains(s, "node-a") || !strings.Contains(s, "unreachable") {
		t.Fatalf("missing skip warning for node-a in output:\n%s", s)
	}
}

// An SSH-enrolled (darwin) pool has no hypervisor node to probe, so updateTarget
// must report ok=false: the update runs unguarded instead of being wrongly
// skipped by a reachability probe against a PVE node that does not exist. A
// hypervisor-backed pool, by contrast, resolves to a node + provider (ok=true).
func TestUpdateTargetGuardsEnrollPools(t *testing.T) {
	e := &Engine{
		Cfg:       &config.Config{Proxmox: config.Proxmox{Node: "node-c"}},
		provCache: map[config.Provider]provider.Provider{config.ProviderProxmox: fakeProvider{}},
	}
	if _, _, ok := e.updateTarget(UpdateChange{Name: "mac-arm64", Spec: config.PoolSpec{OS: "darwin"}}); ok {
		t.Fatal("updateTarget returned ok=true for a darwin (enrolled) pool; it must not probe a hypervisor node")
	}
	if _, _, ok := e.updateTarget(UpdateChange{Name: "linux", Spec: config.PoolSpec{OS: "linux"}}); !ok {
		t.Fatal("updateTarget returned ok=false for a proxmox-backed pool; the reachability guard would never run")
	}
}

// nodeDown classifies only a connectivity failure as down, and memoizes the
// verdict so a dead node is probed once per apply.
func TestNodeDownClassifiesAndMemoizes(t *testing.T) {
	e := &Engine{Cfg: &config.Config{Proxmox: config.Proxmox{Node: "node-c"}}}
	prov := fakeProvider{down: map[string]bool{"node-a": true}}
	cache := reachCache{}
	if !e.nodeDown(context.Background(), prov, "node-a", cache) {
		t.Fatal("nodeDown(node-a) = false, want true (unreachable)")
	}
	if e.nodeDown(context.Background(), prov, "node-b", cache) {
		t.Fatal("nodeDown(node-b) = true, want false (reachable)")
	}
	if !cache["node-a"] || cache["node-b"] {
		t.Fatalf("reach cache not populated as expected: %v", cache)
	}
}
