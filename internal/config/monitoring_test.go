package config

import "testing"

func TestMonitoringResolvedDefaults(t *testing.T) {
	m := MonitoringSpec{}.Resolved()
	if m.Cores != 2 || m.MemoryMB != 4096 || m.DiskGB != 40 {
		t.Errorf("unexpected resource defaults: %+v", m)
	}
	if m.Template != "mayfly-ubuntu-24.04-amd64" {
		t.Errorf("unexpected template default: %q", m.Template)
	}
	if m.RetentionDays != 15 || m.MetricsPort != 9090 {
		t.Errorf("unexpected retention/port defaults: %+v", m)
	}
}

func TestMonitoringEnablesMetrics(t *testing.T) {
	c := &Config{}
	if c.MetricsEnabled() || c.MetricsPort() != 0 {
		t.Fatal("metrics should be off with no monitoring block")
	}
	c.Monitoring = &MonitoringSpec{MetricsPort: 9191}
	if !c.MetricsEnabled() || c.MetricsPort() != 9191 {
		t.Fatalf("metrics should follow the monitoring block: enabled=%v port=%d", c.MetricsEnabled(), c.MetricsPort())
	}
}

func TestNeedsProxmoxWithMonitoringOnly(t *testing.T) {
	// A darwin-only fleet normally skips Proxmox, but a monitoring node is a
	// PVE VM, so its presence must require Proxmox credentials.
	c := &Config{
		Pools:      map[string]PoolSpec{"mac": {Hostname: "m", OS: "darwin", Members: []string{"h"}}},
		Monitoring: &MonitoringSpec{},
	}
	if !c.needsProxmox() {
		t.Fatal("monitoring node should require Proxmox even in a darwin-only fleet")
	}
}

func TestMonitoringHostnameCollision(t *testing.T) {
	c := &Config{
		GitHub:     GitHub{Owner: "o", AppID: 1, InstallationID: 2, PrivateKeyPath: "/k"},
		Proxmox:    Proxmox{URL: "u", TokenID: "t", Secret: "s", Node: "n", Storage: "st"},
		Pools:      map[string]PoolSpec{"p": {Hostname: "dup.example.com", Size: 1}},
		Monitoring: &MonitoringSpec{Hostname: "dup.example.com"},
	}
	if err := c.Validate(); err == nil {
		t.Fatal("expected a hostname-collision error between pool and monitoring")
	}
}
