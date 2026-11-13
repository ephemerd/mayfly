package engine

import (
	"strings"
	"testing"

	"github.com/ephemerd/mayfly/internal/config"
	"github.com/ephemerd/mayfly/internal/state"
)

// dedicatedCacheCfg is a config whose cache runs on its own VLAN-tagged VM,
// alongside a normal monitoring host.
func dedicatedCacheCfg() *config.Config {
	return &config.Config{
		Monitoring: &config.MonitoringSpec{
			Template: "mayfly-ubuntu-24.04-amd64",
			Cores:    2, MemoryMB: 4096, DiskGB: 40,
		},
		RegistryCache: config.RegistryCacheSpec{
			Enabled:   true,
			MaxSizeGB: 20,
			VM: &config.RegistryCacheVMSpec{
				Template: "mayfly-ubuntu-24.04-amd64",
				VLANTag:  2,
				DiskGB:   40,
			},
		},
	}
}

// A cache with its own [registry_cache.vm] must plan a dedicated VM, and the
// utility host must NOT also be told to run one — otherwise the migration
// leaves two caches running.
func TestDiffRegistryCacheVM_CreatesDedicatedAndClearsFromMonitoring(t *testing.T) {
	cfg := dedicatedCacheCfg()
	st := &state.Store{}
	p := &Plan{}

	diffRegistryCacheVM(cfg, st, p)
	if p.RegistryCacheCreate == nil {
		t.Fatal("expected a dedicated cache VM create")
	}
	if got := p.RegistryCacheCreate.Cache.VM.Resolved().VLANTag; got != 2 {
		t.Fatalf("vlan tag = %d, want 2", got)
	}
	if cfg.RegistryCache.OnMonitoring().Enabled {
		t.Fatal("the utility host still thinks it hosts the cache; migration would run two copies")
	}
}

// The migration case: a fleet that has been running the cache colocated adds
// [registry_cache.vm]. The same plan must create the VM AND change the
// monitoring host's config (which is what stops the old copy).
func TestDiffRegistryCacheVM_MigrationAlsoUpdatesMonitoring(t *testing.T) {
	cfg := dedicatedCacheCfg()
	colocatedHash := MonitoringConfigHash(cfg.Monitoring.Resolved(), config.RegistryCacheSpec{
		Enabled: true, MaxSizeGB: 20,
	})
	st := &state.Store{
		Monitoring: &state.Monitoring{
			VMID: 103, PVENode: "node-a", IP: "192.0.2.45",
			Cores: 2, MemoryMB: 4096, DiskGB: 40,
			ConfigHash: colocatedHash,
		},
	}
	p := &Plan{}
	diffMonitoring(cfg, st, p)
	diffRegistryCacheVM(cfg, st, p)

	if p.RegistryCacheCreate == nil {
		t.Fatal("expected the dedicated cache VM to be created")
	}
	if p.MonitoringUpdate == nil || !p.MonitoringUpdate.PushConfig {
		t.Fatal("expected monitoring to be reconfigured so the colocated cache is removed")
	}
	if p.MonitoringUpdate.Cache.Enabled {
		t.Fatal("monitoring update still carries an enabled cache; the old copy would keep running")
	}
}

// Dropping [registry_cache.vm] (or disabling the cache) must destroy the VM
// rather than orphan it.
func TestDiffRegistryCacheVM_DestroysWhenNoLongerDedicated(t *testing.T) {
	for _, tc := range []struct {
		name  string
		cache config.RegistryCacheSpec
	}{
		{"moved back to the utility host", config.RegistryCacheSpec{Enabled: true, MaxSizeGB: 20}},
		{"cache disabled entirely", config.RegistryCacheSpec{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &config.Config{RegistryCache: tc.cache}
			st := &state.Store{RegistryCache: &state.RegistryCache{VMID: 110}}
			p := &Plan{}
			diffRegistryCacheVM(cfg, st, p)
			if !p.RegistryCacheDestroy {
				t.Fatal("expected the dedicated cache VM to be destroyed")
			}
		})
	}
}

// A steady-state fleet must produce no cache work at all — otherwise every
// apply churns the VM.
func TestDiffRegistryCacheVM_NoDriftWhenConverged(t *testing.T) {
	cfg := dedicatedCacheCfg()
	vm := cfg.RegistryCache.VM.Resolved()
	st := &state.Store{RegistryCache: &state.RegistryCache{
		VMID: 110, IP: "192.0.2.150",
		Cores: vm.Cores, MemoryMB: vm.MemoryMB, DiskGB: vm.DiskGB,
		ConfigHash: RegistryCacheConfigHash(cfg.RegistryCache),
	}}
	p := &Plan{}
	diffRegistryCacheVM(cfg, st, p)
	if p.RegistryCacheCreate != nil || p.RegistryCacheUpdate != nil || p.RegistryCacheDestroy {
		t.Fatalf("expected no cache work; got create=%v update=%v destroy=%v",
			p.RegistryCacheCreate != nil, p.RegistryCacheUpdate != nil, p.RegistryCacheDestroy)
	}
	if !p.Empty() {
		t.Fatal("plan should be empty")
	}
}

// The whole point of the split: runners must be handed the cache VM's address
// (on their VLAN), never the utility host's LAN address.
func TestRegistryMirror_UsesDedicatedVMAddress(t *testing.T) {
	cfg := dedicatedCacheCfg()
	st := &state.Store{
		Monitoring:    &state.Monitoring{IP: "192.0.2.45"},
		RegistryCache: &state.RegistryCache{IP: "192.0.2.150"},
	}
	m := registryMirror(cfg, st)
	if !m.Enabled() {
		t.Fatal("expected a mirror")
	}
	if !strings.Contains(m.Endpoint, "192.0.2.150") {
		t.Fatalf("mirror endpoint = %q, want the cache VM's VLAN address", m.Endpoint)
	}
	if strings.Contains(m.Endpoint, "192.0.2.45") {
		t.Fatalf("mirror endpoint = %q — points at the unreachable LAN host, the bug this feature fixes", m.Endpoint)
	}
}

// Until the cache VM has an address there is no mirror to hand out, and the
// plan must say so rather than silently omit it.
func TestRegistryMirror_NoDedicatedAddressYetWarns(t *testing.T) {
	cfg := dedicatedCacheCfg()
	st := &state.Store{Monitoring: &state.Monitoring{IP: "192.0.2.45"}}
	if m := registryMirror(cfg, st); m.Enabled() {
		t.Fatalf("expected no mirror before the cache VM exists, got %q", m.Endpoint)
	}
	p := &Plan{}
	warnRegistryCache(cfg, st, p)
	if len(p.Warnings) != 1 || !strings.Contains(p.Warnings[0], "dedicated cache VM") {
		t.Fatalf("expected a dedicated-VM lag warning, got %v", p.Warnings)
	}
}

// The plan is how an operator decides whether to apply, so the cache VM's
// create must actually show up in it -- and must say which VLAN it lands on,
// since an untagged NIC is the failure this feature exists to prevent.
func TestPlanRender_DedicatedCacheVM(t *testing.T) {
	cfg := dedicatedCacheCfg()
	p := &Plan{}
	diffRegistryCacheVM(cfg, &state.Store{}, p)

	var sb strings.Builder
	p.Render(&sb)
	out := sb.String()

	for _, want := range []string{"+ registry_cache", "vlan 2", "1 to add"} {
		if !strings.Contains(out, want) {
			t.Fatalf("plan output missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "No changes") {
		t.Fatalf("plan reported no changes while creating a cache VM:\n%s", out)
	}
}

// An untagged cache VM is legal but is almost certainly a mistake, so the plan
// has to call it out rather than quietly provision an unreachable cache.
func TestPlanRender_UntaggedCacheVMIsFlagged(t *testing.T) {
	cfg := dedicatedCacheCfg()
	cfg.RegistryCache.VM.VLANTag = 0
	p := &Plan{}
	diffRegistryCacheVM(cfg, &state.Store{}, p)

	var sb strings.Builder
	p.Render(&sb)
	if out := sb.String(); !strings.Contains(out, "UNTAGGED") {
		t.Fatalf("expected an untagged-vnic warning in the plan:\n%s", out)
	}
}

// Colocated fleets must be completely unaffected by this feature.
func TestRegistryCache_ColocatedUnchanged(t *testing.T) {
	cfg := &config.Config{
		Monitoring:    &config.MonitoringSpec{Template: "t", Cores: 2, MemoryMB: 4096, DiskGB: 40},
		RegistryCache: config.RegistryCacheSpec{Enabled: true, MaxSizeGB: 20},
	}
	if cfg.RegistryCache.Dedicated() {
		t.Fatal("cache without a [registry_cache.vm] must not be dedicated")
	}
	if !cfg.RegistryCache.OnMonitoring().Enabled {
		t.Fatal("a colocated cache must still be rendered onto the utility host")
	}
	st := &state.Store{Monitoring: &state.Monitoring{IP: "192.0.2.45"}}
	if m := registryMirror(cfg, st); !strings.Contains(m.Endpoint, "192.0.2.45") {
		t.Fatalf("colocated mirror = %q, want the utility host address", m.Endpoint)
	}
	p := &Plan{}
	diffRegistryCacheVM(cfg, st, p)
	if p.RegistryCacheCreate != nil || p.RegistryCacheDestroy {
		t.Fatal("a colocated cache must not plan any dedicated-VM work")
	}
}
