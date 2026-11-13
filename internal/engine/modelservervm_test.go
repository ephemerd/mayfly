package engine

import (
	"bytes"
	"strings"
	"testing"

	"github.com/ephemerd/mayfly/internal/config"
	"github.com/ephemerd/mayfly/internal/state"
)

func modelServerCfg() *config.Config {
	return &config.Config{
		Proxmox: config.Proxmox{TokenID: "root@pam!mayfly"},
		ModelServer: config.ModelServerSpec{
			Enabled: true,
			VM: &config.ModelServerVMSpec{
				Node:     "node-b",
				Template: "mayfly-ubuntu-24.04-amd64",
				VLANTag:  2,
				HostPCI:  []string{"p40"},
			},
		},
	}
}

// recorded is the state a freshly provisioned server from cfg leaves.
func recorded(cfg *config.Config) *state.ModelServer {
	vm := cfg.ModelServer.VM.Resolved()
	return &state.ModelServer{
		VMID: 105, Hostname: "mfl-model-server-105", PVENode: "node-b", IP: "192.0.2.50",
		Cores: vm.Cores, MemoryMB: vm.MemoryMB, DiskGB: vm.DiskGB,
		Template: vm.Template, VLANTag: vm.VLANTag, HostPCI: vm.HostPCI,
		ConfigHash: ModelServerConfigHash(cfg.ModelServer),
	}
}

func TestDiffModelServerVM_Create(t *testing.T) {
	p := &Plan{}
	diffModelServerVM(modelServerCfg(), &state.Store{}, p)
	if p.ModelServerCreate == nil || p.ModelServerDestroy || p.ModelServerUpdate != nil {
		t.Fatalf("plan = create %v update %v destroy %v; want only a create",
			p.ModelServerCreate, p.ModelServerUpdate, p.ModelServerDestroy)
	}
}

func TestDiffModelServerVM_InSyncIsEmpty(t *testing.T) {
	cfg := modelServerCfg()
	p := &Plan{}
	diffModelServerVM(cfg, &state.Store{ModelServer: recorded(cfg)}, p)
	if !p.Empty() {
		t.Fatalf("a server matching its config planned work: %+v", p)
	}
}

func TestDiffModelServerVM_UpdateInPlace(t *testing.T) {
	cfg := modelServerCfg()
	st := &state.Store{ModelServer: recorded(cfg)}
	cfg.ModelServer.Models = []string{"qwen3:14b"}
	cfg.ModelServer.VM.MemoryMB = 12288
	cfg.ModelServer.VM.DiskGB = 150

	p := &Plan{}
	diffModelServerVM(cfg, st, p)
	u := p.ModelServerUpdate
	if u == nil || p.ModelServerCreate != nil || p.ModelServerDestroy {
		t.Fatalf("want only an in-place update, got %+v", p)
	}
	if !u.PushConfig {
		t.Error("a model list change did not push config")
	}
	if !u.PowerCycle {
		t.Error("a memory change did not power cycle")
	}
	if u.DiskGrowGB != 150 {
		t.Errorf("DiskGrowGB = %d, want 150", u.DiskGrowGB)
	}
}

// Moving the GPU must replace the VM, never edit it live, and the plan has
// to carry the destroy so apply frees the card before the create claims it.
func TestDiffModelServerVM_HostPCIChangeReplaces(t *testing.T) {
	cfg := modelServerCfg()
	st := &state.Store{ModelServer: recorded(cfg)}
	cfg.ModelServer.VM.HostPCI = []string{"p40-slot2"}

	p := &Plan{}
	diffModelServerVM(cfg, st, p)
	if !p.ModelServerDestroy || p.ModelServerCreate == nil || p.ModelServerUpdate != nil {
		t.Fatalf("want destroy + create, got create %v update %v destroy %v",
			p.ModelServerCreate, p.ModelServerUpdate, p.ModelServerDestroy)
	}
}

func TestDiffModelServerVM_DisableDestroys(t *testing.T) {
	cfg := modelServerCfg()
	st := &state.Store{ModelServer: recorded(cfg)}
	cfg.ModelServer.Enabled = false

	p := &Plan{}
	diffModelServerVM(cfg, st, p)
	if !p.ModelServerDestroy || p.ModelServerCreate != nil {
		t.Fatalf("disabling the server did not plan a destroy: %+v", p)
	}
	if _, _, destroys := p.Counts(); destroys != 1 {
		t.Errorf("Counts destroys = %d, want 1", destroys)
	}
}

// Token auth cannot pass a raw address through; the plan must say so before
// apply hits Proxmox's refusal.
func TestDiffModelServerVM_WarnsOnRawAddressWithToken(t *testing.T) {
	cfg := modelServerCfg()
	cfg.ModelServer.VM.HostPCI = []string{"0000:07:00.0"}
	p := &Plan{}
	diffModelServerVM(cfg, &state.Store{}, p)
	if len(p.Warnings) != 1 || !strings.Contains(p.Warnings[0], "Resource Mappings") {
		t.Fatalf("warnings = %v, want the resource-mapping hint", p.Warnings)
	}

	cfg = modelServerCfg() // mapping name: nothing to warn about
	p = &Plan{}
	diffModelServerVM(cfg, &state.Store{}, p)
	if len(p.Warnings) != 0 {
		t.Errorf("mapping name produced warnings: %v", p.Warnings)
	}
}

func TestRenderModelServerCreate(t *testing.T) {
	p := &Plan{}
	diffModelServerVM(modelServerCfg(), &state.Store{}, p)
	var buf bytes.Buffer
	p.Render(&buf)
	out := buf.String()
	for _, want := range []string{"+ model_server", "hostpci [p40]", "pinned", ":11434", "qwen3:8b", "driver 580"} {
		if !strings.Contains(out, want) {
			t.Errorf("render missing %q:\n%s", want, out)
		}
	}
}

func TestHostPCIDevices(t *testing.T) {
	devs := hostPCIDevices([]string{"0000:07:00.0", "p40"})
	if devs[0].Host != "0000:07:00.0" || devs[0].Mapping != "" {
		t.Errorf("address entry = %+v", devs[0])
	}
	if devs[1].Mapping != "p40" || devs[1].Host != "" {
		t.Errorf("mapping entry = %+v", devs[1])
	}
}

func TestResolveServiceVMTarget_ModelServer(t *testing.T) {
	st := &state.Store{ModelServer: recorded(modelServerCfg())}
	for _, name := range []string{"model-server", "mfl-model-server-105"} {
		tgt, ok := ResolveServiceVMTarget(st, name)
		if !ok || tgt.ID != 105 || tgt.Node != "node-b" {
			t.Errorf("ResolveServiceVMTarget(%q) = %+v, %v", name, tgt, ok)
		}
	}
}
