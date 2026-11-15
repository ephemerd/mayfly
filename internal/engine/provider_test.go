package engine

import (
	"testing"

	"github.com/luthermonson/mayfly/internal/config"
	"github.com/luthermonson/mayfly/internal/provider"
)

// newSelectorEngine builds a bare Engine with just the config + provider cache
// wired — enough to exercise the per-pool provider selector without touching
// GitHub / Cloudflare / state.
func newSelectorEngine(cfg *config.Config) *Engine {
	return &Engine{Cfg: cfg, provCache: map[config.Provider]provider.Provider{}}
}

// A proxmox (or default) pool resolves to the Proxmox backend, and the same
// backend instance is cached and reused across calls.
func TestProviderForProxmoxAndCache(t *testing.T) {
	cfg := &config.Config{
		Proxmox: config.Proxmox{URL: "https://pve:8006", TokenID: "t", Secret: "s", Node: "n", Storage: "st"},
		Pools: map[string]config.PoolSpec{
			"x86":     {Hostname: "a", Size: 1},                      // default proxmox
			"x86-exp": {Hostname: "b", Size: 1, Provider: "proxmox"}, // explicit proxmox
		},
	}
	e := newSelectorEngine(cfg)

	p1, err := e.providerFor(cfg.Pools["x86"])
	if err != nil {
		t.Fatalf("providerFor(default) = %v, want nil", err)
	}
	if _, ok := p1.(*provider.Proxmox); !ok {
		t.Fatalf("default pool resolved to %T, want *provider.Proxmox", p1)
	}

	p2, err := e.providerFor(cfg.Pools["x86-exp"])
	if err != nil {
		t.Fatalf("providerFor(proxmox) = %v, want nil", err)
	}
	if p1 != p2 {
		t.Error("two proxmox pools must share the same cached backend instance")
	}
	if got := len(e.provCache); got != 1 {
		t.Errorf("provCache has %d entries, want 1 (proxmox only)", got)
	}
}

// monitoringProvider always resolves to Proxmox.
func TestMonitoringProviderIsProxmox(t *testing.T) {
	cfg := &config.Config{
		Proxmox: config.Proxmox{URL: "https://pve:8006", TokenID: "t", Secret: "s", Node: "n", Storage: "st"},
		Pools: map[string]config.PoolSpec{
			"arm": {Hostname: "c", Size: 1, Arch: "arm64", Node: "node-b"},
		},
	}
	e := newSelectorEngine(cfg)

	p, err := e.monitoringProvider()
	if err != nil {
		t.Fatalf("monitoringProvider() = %v, want nil", err)
	}
	if _, ok := p.(*provider.Proxmox); !ok {
		t.Fatalf("monitoring resolved to %T, want *provider.Proxmox", p)
	}
}

// providerForPool defaults to Proxmox for a pool no longer in config (e.g.
// mid-destroy of a removed pool).
func TestProviderForPoolUnknownDefaultsProxmox(t *testing.T) {
	cfg := &config.Config{
		Proxmox: config.Proxmox{URL: "https://pve:8006", TokenID: "t", Secret: "s", Node: "n", Storage: "st"},
		Pools:   map[string]config.PoolSpec{},
	}
	e := newSelectorEngine(cfg)

	p, err := e.providerForPool("gone")
	if err != nil {
		t.Fatalf("providerForPool(unknown) = %v, want nil", err)
	}
	if _, ok := p.(*provider.Proxmox); !ok {
		t.Fatalf("unknown pool resolved to %T, want *provider.Proxmox", p)
	}
}
