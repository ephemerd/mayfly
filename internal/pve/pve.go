package pve

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/luthermonson/go-proxmox"
	"github.com/luthermonson/mayfly/internal/config"
)

const (
	TagMayfly   = "mayfly"
	TagEphemerd = "ephemerd"
)

// ErrHostUnreachable marks a failure that means we could not reach a PVE node's
// API to determine anything — the host is down, the API errored, or it returned
// a truncated/empty body (e.g. "unexpected end of JSON input"). It is wrapped
// into such errors so best-effort callers can tell "couldn't check" apart from
// a definitive negative verdict. A node that can't be reached also can't be
// reconfigured, so those callers may warn and skip rather than hard-fail.
var ErrHostUnreachable = errors.New("proxmox host unreachable")

type Client struct {
	API  *proxmox.Client
	Node string
	cfg  config.Proxmox
}

func New(cfg config.Proxmox) *Client {
	opts := []proxmox.Option{
		proxmox.WithAPIToken(cfg.TokenID, cfg.Secret),
		proxmox.WithTimeout(30 * time.Second),
	}
	if cfg.Insecure {
		opts = append(opts, proxmox.WithInsecureSkipVerify())
	}
	// Multi-endpoint failover (go-proxmox v0.9.0+): when extra cluster
	// endpoints are configured, the client fails over to them when the primary
	// (cfg.URL) is unreachable or returns a gateway-class error. Any node in a
	// quorate cluster serves the whole API and tokens are cluster-wide, so a
	// single down node no longer blocks cluster-wide calls or aborts an apply.
	// With no extra endpoints this is a no-op and the client talks only to
	// cfg.URL, exactly as before.
	if len(cfg.Endpoints) > 0 {
		opts = append(opts, proxmox.WithAdditionalEndpoints(cfg.Endpoints...))
		if cfg.EndpointFailureThreshold > 0 {
			opts = append(opts, proxmox.WithEndpointFailureThreshold(cfg.EndpointFailureThreshold))
		}
	}
	api := proxmox.NewClient(cfg.URL, opts...)
	return &Client{API: api, Node: cfg.Node, cfg: cfg}
}

func (c *Client) DefaultNode(ctx context.Context) (*proxmox.Node, error) {
	return c.API.Node(ctx, c.Node)
}

// NodeByName resolves a PVE node, falling back to the configured default
// when name is empty.
func (c *Client) NodeByName(ctx context.Context, name string) (*proxmox.Node, error) {
	if name == "" {
		name = c.Node
	}
	return c.API.Node(ctx, name)
}

func (c *Client) NextID(ctx context.Context) (int, error) {
	cluster, err := c.API.Cluster(ctx)
	if err != nil {
		return 0, err
	}
	return cluster.NextID(ctx)
}

// FindTemplate walks VMs on the named node (default node when empty) and
// returns the one whose Name matches and whose Template flag is set.
func (c *Client) FindTemplate(ctx context.Context, nodeName, name string) (*proxmox.VirtualMachine, error) {
	node, err := c.NodeByName(ctx, nodeName)
	if err != nil {
		return nil, err
	}
	vms, err := node.VirtualMachines(ctx)
	if err != nil {
		return nil, err
	}
	for _, v := range vms {
		if v.Name == name && bool(v.Template) {
			return node.VirtualMachine(ctx, int(v.VMID))
		}
	}
	return nil, fmt.Errorf("template %q not found on node %q", name, node.Name)
}

// PrimaryIPv4 returns the guest's first non-loopback, non-link-local IPv4
// address via the QEMU guest agent — the LAN address other lab hosts (e.g.
// the Prometheus node) use to reach it. Requires the agent to be up.
func PrimaryIPv4(ctx context.Context, vm *proxmox.VirtualMachine) (string, error) {
	ifaces, err := vm.AgentGetNetworkIFaces(ctx)
	if err != nil {
		return "", err
	}
	for _, iface := range ifaces {
		for _, addr := range iface.IPAddresses {
			if addr.IPAddressType != "ipv4" {
				continue
			}
			ip := addr.IPAddress
			if ip == "" || strings.HasPrefix(ip, "127.") || strings.HasPrefix(ip, "169.254.") {
				continue
			}
			return ip, nil
		}
	}
	return "", fmt.Errorf("no usable IPv4 address reported by guest agent")
}

// BridgeVLANAware reports whether the named host bridge on the given PVE node
// (empty = default node) is configured VLAN-aware — "bridge-vlan-aware yes" in
// /etc/network/interfaces, which the API surfaces as bridge_vlan_aware=1.
//
// This is the linchpin of the per-pool vlan_tag feature. Proxmox will happily
// write net0 "tag=<n>" onto a plain (non-VLAN-aware) bridge and start the VM,
// but the tag is inert there: the guest lands on the untagged LAN, not the
// isolated VLAN. Callers use this to fail loud before provisioning a runner
// that only thinks it is isolated.
func (c *Client) BridgeVLANAware(ctx context.Context, nodeName, bridge string) (bool, error) {
	node, err := c.NodeByName(ctx, nodeName)
	if err != nil {
		// NodeByName resolves the node via GET /nodes/<name>/status — the first
		// call to touch the host. This is where an unreachable/down node fails:
		// connection refused, timeout, or the truncated "unexpected end of JSON
		// input" body a flapping node returns. We learned nothing about the
		// bridge, and a node we can't reach can't be reconfigured anyway, so flag
		// it unreachable — best-effort callers (the vlan_tag preflight) then warn
		// and skip rather than abort. The sentinel is attached on the connectivity
		// boundary — here and in NodeReachable below — never on the bridge read.
		return false, fmt.Errorf("%w: node %q: %v", ErrHostUnreachable, nodeName, err)
	}
	nw, err := node.Network(ctx, bridge)
	if err != nil {
		// The node answered but reading this specific bridge failed. Proxmox
		// errors here when the interface genuinely does not exist (a typo'd,
		// renamed, or deleted bridge in the template's net0) — a real
		// misconfiguration, NOT a reachability problem. Keep it a HARD error
		// (no ErrHostUnreachable) so the preflight can't silently skip a pool
		// whose bridge is missing: that would be a sibling of the exact
		// fail-open the guard exists to prevent.
		return false, fmt.Errorf("read bridge %q on node %q: %w", bridge, node.Name, err)
	}
	if nw == nil {
		// Defensive: a 200 with an empty/null body would leave nw nil without an
		// error. A missing bridge is a hard error, same as the errored read.
		return false, fmt.Errorf("bridge %q not found on node %q", bridge, node.Name)
	}
	return nw.BridgeVLANAware == 1, nil
}

// NodeReachable probes whether a node's PVE API answers at all, via the same
// GET /nodes/<name>/status boundary BridgeVLANAware uses (NodeByName). It
// returns nil when the node answers, or an error wrapping ErrHostUnreachable
// when it cannot be reached — down, connection refused, or the truncated
// "unexpected end of JSON input" body a flapping node returns. Best-effort
// callers (apply's maintenance ops against an existing node) use it to warn and
// skip a node that is simply down, instead of aborting reconciliation for the
// whole fleet. It makes no claim beyond reachability: a node that answers here
// may still fail a later specific call, which stays that call's hard error.
func (c *Client) NodeReachable(ctx context.Context, nodeName string) error {
	if _, err := c.NodeByName(ctx, nodeName); err != nil {
		return fmt.Errorf("%w: node %q: %v", ErrHostUnreachable, nodeName, err)
	}
	return nil
}

// StorageByName resolves a storage by name on the default node.
func (c *Client) StorageByName(ctx context.Context, name string) (*proxmox.Storage, error) {
	node, err := c.DefaultNode(ctx)
	if err != nil {
		return nil, err
	}
	return node.Storage(ctx, name)
}
