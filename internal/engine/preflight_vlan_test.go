package engine

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/ephemerd/mayfly/internal/config"
	"github.com/ephemerd/mayfly/internal/provider"
	"github.com/ephemerd/mayfly/internal/pve"
)

// fakeProvider is a stub provider.Provider whose only meaningful method is
// VLANBridgeStatus; the preflight never touches the rest. It lets the
// engine-level vlan_tag preflight be driven without a live Proxmox.
type fakeProvider struct {
	bridge string
	aware  bool
	err    error
	// down names nodes whose NodeReachable returns pve.ErrHostUnreachable; all
	// others are reachable. nil = every node reachable (the common case).
	down map[string]bool
}

func (f fakeProvider) EnsureTemplate(context.Context, provider.TemplateSpec) error { return nil }
func (f fakeProvider) TemplateExists(context.Context, string, string) (bool, error) {
	return true, nil
}
func (f fakeProvider) NextID(context.Context) (int, error) { return 0, nil }
func (f fakeProvider) CreateInstance(context.Context, provider.CreateSpec) (provider.Instance, error) {
	return nil, nil
}
func (f fakeProvider) GetInstance(context.Context, string, int) (provider.Instance, error) {
	return nil, nil
}
func (f fakeProvider) ListInstances(context.Context, string) ([]provider.InstanceInfo, error) {
	return nil, nil
}
func (f fakeProvider) VLANBridgeStatus(context.Context, string, string) (string, bool, error) {
	return f.bridge, f.aware, f.err
}
func (f fakeProvider) NodeReachable(_ context.Context, node string) error {
	if f.down[node] {
		return fmt.Errorf("%w: node %q", pve.ErrHostUnreachable, node)
	}
	return nil
}

// preflightVLAN must fail soft when a tagged pool's host is unreachable (the
// down-node case that used to abort every plan/apply fleet-wide) yet still fail
// hard when a reachable bridge is genuinely not VLAN-aware (the silent
// fail-open the check exists to catch).
func TestPreflightVLANUnreachableFailsSoft(t *testing.T) {
	tests := []struct {
		name        string
		status      fakeProvider
		wantErr     bool
		wantWarnSub string // substring expected in the warning output (only when !wantErr)
	}{
		{
			name:        "unreachable host warns and skips",
			status:      fakeProvider{bridge: "vmbr0", err: fmt.Errorf("read node: %w", pve.ErrHostUnreachable)},
			wantErr:     false,
			wantWarnSub: "unreachable",
		},
		{
			name:    "reachable non-vlan-aware bridge still hard-fails",
			status:  fakeProvider{bridge: "vmbr0", aware: false},
			wantErr: true,
		},
		{
			name:    "reachable vlan-aware bridge passes",
			status:  fakeProvider{bridge: "vmbr0", aware: true},
			wantErr: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &config.Config{
				Proxmox: config.Proxmox{Node: "node-a"},
				Pools: map[string]config.PoolSpec{
					"linux-amd64": {Hostname: "ci.example.com", Size: 1, VLANTag: 2},
				},
			}
			var out bytes.Buffer
			e := &Engine{
				Cfg:       cfg,
				Out:       &out,
				provCache: map[config.Provider]provider.Provider{config.ProviderProxmox: tt.status},
			}

			err := e.preflightVLAN(context.Background())
			if (err != nil) != tt.wantErr {
				t.Fatalf("preflightVLAN() err=%v, wantErr=%v", err, tt.wantErr)
			}
			if tt.wantErr {
				return
			}
			if tt.wantWarnSub != "" && !strings.Contains(out.String(), tt.wantWarnSub) {
				t.Errorf("expected warning containing %q, got %q", tt.wantWarnSub, out.String())
			}
		})
	}
}

// The unreachable classification must survive error wrapping: preflight checks
// with errors.Is, so a sentinel buried several %w layers deep still fails soft.
func TestPreflightVLANUnreachableIsWrapped(t *testing.T) {
	wrapped := fmt.Errorf("outer: %w", fmt.Errorf("inner: %w", pve.ErrHostUnreachable))
	if !errors.Is(wrapped, pve.ErrHostUnreachable) {
		t.Fatal("sanity: wrapped error must satisfy errors.Is(pve.ErrHostUnreachable)")
	}
	cfg := &config.Config{
		Proxmox: config.Proxmox{Node: "node-a"},
		Pools: map[string]config.PoolSpec{
			"linux-amd64": {Hostname: "ci.example.com", Size: 1, VLANTag: 2},
		},
	}
	var out bytes.Buffer
	e := &Engine{
		Cfg:       cfg,
		Out:       &out,
		provCache: map[config.Provider]provider.Provider{config.ProviderProxmox: fakeProvider{bridge: "vmbr0", err: wrapped}},
	}
	if err := e.preflightVLAN(context.Background()); err != nil {
		t.Fatalf("preflightVLAN() with wrapped unreachable error = %v, want nil (skip)", err)
	}
}
