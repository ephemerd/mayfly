package engine

import (
	"testing"

	"github.com/luthermonson/mayfly/internal/config"
	"github.com/luthermonson/mayfly/internal/monitoring"
	"github.com/luthermonson/mayfly/internal/state"
)

func monCfg() *config.Config {
	return &config.Config{
		GitHub:     config.GitHub{Owner: "ephpm", AppID: 1, InstallationID: 2, PrivateKeyPath: "/k"},
		Proxmox:    config.Proxmox{Node: "node-a", Storage: "local"},
		Monitoring: &config.MonitoringSpec{},
	}
}

func TestDiffMonitoringCreate(t *testing.T) {
	cfg := monCfg()
	st := &state.Store{}
	p := Diff(cfg, st, []byte("pem"), nil, "")
	if p.MonitoringCreate == nil {
		t.Fatal("expected a monitoring create")
	}
	if p.MonitoringUpdate != nil || p.MonitoringDestroy {
		t.Fatal("unexpected update/destroy alongside create")
	}
	if p.MonitoringCreate.Spec.Cores != 2 || p.MonitoringCreate.Spec.MetricsPort != 9090 {
		t.Errorf("spec defaults not resolved: %+v", p.MonitoringCreate.Spec)
	}
}

func TestDiffMonitoringDestroyWhenRemoved(t *testing.T) {
	cfg := monCfg()
	cfg.Monitoring = nil // removed from config
	st := &state.Store{Monitoring: &state.Monitoring{VMID: 200}}
	p := Diff(cfg, st, []byte("pem"), nil, "")
	if !p.MonitoringDestroy {
		t.Fatal("expected monitoring destroy when removed from config")
	}
}

func TestDiffMonitoringNoChangeWhenConverged(t *testing.T) {
	cfg := monCfg()
	spec := cfg.Monitoring.Resolved()
	st := &state.Store{Monitoring: &state.Monitoring{
		VMID:       200,
		Cores:      spec.Cores,
		MemoryMB:   spec.MemoryMB,
		DiskGB:     spec.DiskGB,
		Template:   spec.Template,
		ConfigHash: MonitoringConfigHash(spec, config.RegistryCacheSpec{}),
		TargetHash: monitoring.TargetsHash(&state.Store{}, spec.MetricsPort),
	}}
	p := Diff(cfg, st, []byte("pem"), nil, "")
	if p.MonitoringUpdate != nil {
		t.Fatalf("expected no monitoring update when converged, got %+v", p.MonitoringUpdate)
	}
	if !p.Empty() {
		t.Fatal("plan should be empty when everything is converged")
	}
}

func TestDiffMonitoringResourceDrift(t *testing.T) {
	cfg := monCfg()
	cfg.Monitoring.Cores = 4 // bump from default 2
	spec := cfg.Monitoring.Resolved()
	st := &state.Store{Monitoring: &state.Monitoring{
		VMID:       200,
		Cores:      2,
		MemoryMB:   spec.MemoryMB,
		DiskGB:     spec.DiskGB,
		Template:   spec.Template,
		ConfigHash: MonitoringConfigHash(spec, config.RegistryCacheSpec{}),
		TargetHash: monitoring.TargetsHash(&state.Store{}, spec.MetricsPort),
	}}
	p := Diff(cfg, st, []byte("pem"), nil, "")
	if p.MonitoringUpdate == nil || !p.MonitoringUpdate.PowerCycle {
		t.Fatalf("expected a power-cycle update for the core bump, got %+v", p.MonitoringUpdate)
	}
}
