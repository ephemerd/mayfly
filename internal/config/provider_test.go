package config

import (
	"strings"
	"testing"
)

func TestResolvedProviderDefaultsToProxmox(t *testing.T) {
	p := PoolSpec{Hostname: "ci.example.com", Size: 1} // no provider field
	if got := p.ResolvedProvider(); got != ProviderProxmox {
		t.Errorf("ResolvedProvider() = %q, want proxmox (backward compatible)", got)
	}
}

func TestResolvedProviderExplicitProxmox(t *testing.T) {
	p := PoolSpec{Hostname: "ci.example.com", Size: 1, Provider: "proxmox"}
	if got := p.ResolvedProvider(); got != ProviderProxmox {
		t.Errorf("ResolvedProvider() = %q, want proxmox", got)
	}
}

// ResolvedPlacement falls back to the global [proxmox] node/storage and lets a
// per-pool override win.
func TestResolvedPlacement(t *testing.T) {
	px := Proxmox{Node: "node-a", Storage: "local-lvm"}

	t.Run("inherits proxmox defaults", func(t *testing.T) {
		p := PoolSpec{}
		node, storage := p.ResolvedPlacement(px)
		if node != "node-a" || storage != "local-lvm" {
			t.Errorf("got %q/%q, want node-a/local-lvm", node, storage)
		}
	})

	t.Run("per-pool overrides win", func(t *testing.T) {
		p := PoolSpec{Node: "node-c", Storage: "fast"}
		node, storage := p.ResolvedPlacement(px)
		if node != "node-c" || storage != "fast" {
			t.Errorf("got %q/%q, want node-c/fast", node, storage)
		}
	})
}

// A VM pool requires Proxmox creds; an enrolled darwin pool does not.
func TestNeedsProxmox(t *testing.T) {
	vm := validBase()
	vm.Pools = map[string]PoolSpec{
		"x86": {Hostname: "b.example.com", Size: 1}, // default proxmox
	}
	if !vm.needsProxmox() {
		t.Error("a VM pool must require proxmox")
	}

	enrolled := validBase()
	enrolled.Proxmox = Proxmox{}
	enrolled.Pools = map[string]PoolSpec{
		"macs": {Hostname: "d.example.com", OS: "darwin", Members: []string{"m1"}},
	}
	if enrolled.needsProxmox() {
		t.Error("darwin-only fleet must not require proxmox")
	}
}

func TestInvalidProviderValue(t *testing.T) {
	c := validBase()
	c.Pools = map[string]PoolSpec{
		"x": {Hostname: "a.example.com", Size: 1, Provider: "vmware"},
	}
	err := c.Validate()
	if err == nil || !strings.Contains(err.Error(), "provider must be") {
		t.Fatalf("Validate() = %v, want invalid-provider error", err)
	}
}

// Incus is retired: provider = "incus" is now a hard validation error, not a
// silent fallback to another backend.
func TestIncusProviderRejected(t *testing.T) {
	c := validBase()
	c.Pools = map[string]PoolSpec{
		"arm": {Hostname: "a.example.com", Size: 1, Arch: "arm64", Provider: "incus"},
	}
	err := c.Validate()
	if err == nil || !strings.Contains(err.Error(), "provider must be") {
		t.Fatalf("Validate() = %v, want provider = \"incus\" rejected", err)
	}
}

func TestProviderRejectedOnEnrolledPool(t *testing.T) {
	c := validBase()
	c.Pools = map[string]PoolSpec{
		"macs": {Hostname: "a.example.com", OS: "darwin", Members: []string{"m1"}, Provider: "proxmox"},
	}
	err := c.Validate()
	if err == nil || !strings.Contains(err.Error(), "provider does not apply") {
		t.Fatalf("Validate() = %v, want provider-not-applicable error", err)
	}
}
