package engine

// Provider-aware node operations.
//
// `mayfly node status|exec|drain|upgrade` historically hard-coded the Proxmox
// guest-agent channel in a way that made SSH-enrolled bare metal unreachable —
// an enrolled Mac has no hypervisor id at all (0), so feeding its recorded id
// to the Proxmox API as a VMID went nowhere. Only `apply` worked on those
// nodes, because the apply path had already grown both a hypervisor abstraction
// (internal/provider) and an SSH transport (SSHRun). The node commands simply
// never used them.
//
// The shape here closes that gap with two pieces:
//
//   - ResolveNodeTarget is a PURE function of config + state. It answers "how
//     is this node reached" without opening a connection, so the routing
//     decision — the part that was wrong — is unit-testable against a table of
//     fleets rather than against a live daemon.
//   - NewNodeExecutor turns a target into a live NodeExecutor. It is the only
//     place that dials anything.
//
// There are two executors: the hypervisor channel (Proxmox guest agent, via
// provider.Instance.Exec) and bare-metal SSH. The two are genuinely different
// because SSH has no hypervisor underneath it at all.
//
// Subcommands are written once against NodeExecutor; none of them branch on
// transport.

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"

	gossh "golang.org/x/crypto/ssh"

	"github.com/luthermonson/mayfly/internal/config"
	"github.com/luthermonson/mayfly/internal/provider"
	"github.com/luthermonson/mayfly/internal/state"
)

// NodeTransport is the channel mayfly runs a command through on a node.
type NodeTransport string

const (
	// NodeTransportHypervisor goes through the pool's hypervisor backend's
	// in-guest exec channel — the qemu-guest-agent on Proxmox.
	NodeTransportHypervisor NodeTransport = "hypervisor"
	// NodeTransportSSH reaches an SSH-enrolled bare-metal member (a darwin Mac
	// mini, or a linux box named in a pool's members list). There is no
	// hypervisor to ask, so the transport is SSH under sudo.
	NodeTransportSSH NodeTransport = "ssh"
)

// NodeTarget is the fully resolved, connection-free description of how to reach
// one fleet node. Everything needed to dial is captured here so resolution can
// be tested and printed without side effects.
type NodeTarget struct {
	Transport NodeTransport
	// Provider is the hypervisor backend, meaningful only for
	// NodeTransportHypervisor.
	Provider config.Provider
	// Node is the backend node the instance sits on (Proxmox node name), and
	// ID the backend instance id. Hypervisor transport only.
	Node string
	ID   int
	// SSH carries the credentials for NodeTransportSSH.
	SSH SSHTarget
	// Name/OS/Arch describe the node itself, for command building and errors.
	Name string
	OS   string
	Arch string
}

// ResolveNodeTarget decides how to reach member of pool. Pure: no I/O, no
// connections.
//
// The pool's configured provider is authoritative, because that is what the
// apply path used to build the node in the first place. Two fallbacks keep the
// command usable when config and state disagree:
//
//   - A pool no longer in config (mid-teardown, or removed by hand) resolves by
//     state alone — SSH when no backend id was ever recorded, Proxmox
//     otherwise, matching Engine.providerForPool's fallback.
//   - A member with no backend id is always SSH, whatever the config says. A
//     zero id is the enrollment signature: mayfly never allocates 0 for a VM,
//     and an enrolled host has nothing to look up on a hypervisor.
func ResolveNodeTarget(cfg *config.Config, pool *state.Pool, member *state.Member) NodeTarget {
	t := NodeTarget{Name: member.Hostname, OS: member.OS, Arch: member.Arch}
	spec, inConfig := resolvedPoolSpec(cfg, pool.Name)

	if member.VMID == 0 || (inConfig && spec.UsesEnroll()) {
		t.Transport = NodeTransportSSH
		t.SSH = nodeSSHTarget(pool, spec, member.Hostname)
		return t
	}

	t.Transport = NodeTransportHypervisor
	t.Provider = config.ProviderProxmox
	if inConfig {
		t.Provider = spec.ResolvedProvider()
	}
	t.Node = member.PVENode
	t.ID = member.VMID
	return t
}

// resolvedPoolSpec looks a pool's spec up in config with defaults applied.
// Reports false when the pool is not (or no longer) configured.
func resolvedPoolSpec(cfg *config.Config, name string) (config.PoolSpec, bool) {
	raw, ok := cfg.Pools[name]
	if !ok {
		return config.PoolSpec{}, false
	}
	return raw.Resolved(cfg.Defaults), true
}

// nodeSSHTarget builds the SSH credentials for an enrolled member.
//
// State wins: apply converges the pool's transport settings into state on every
// run (updatePool), and state still resolves for a pool that has since been
// removed from the config — which is exactly when an operator needs to reach
// the box. The config spec only fills fields state predates, so a state file
// written before ssh_user/ssh_key were recorded still works.
func nodeSSHTarget(pool *state.Pool, spec config.PoolSpec, host string) SSHTarget {
	t := sshTarget(pool, host)
	if t.User == "" {
		t.User = spec.SSHUser
	}
	if t.KeyPath == "" {
		t.KeyPath = spec.SSHKey
	}
	if t.Port == 0 {
		t.Port = spec.SSHPort
	}
	if t.Password == "" {
		t.Password = spec.SSHPassword
	}
	return t
}

// NodeExecResult is the outcome of running one command on a node.
type NodeExecResult struct {
	Stdout   string
	Stderr   string
	ExitCode int
}

// NodeExecutor runs commands on one already-resolved node. A non-nil error
// means the command could not be run (unreachable node, dead daemon, timeout);
// a command that ran and failed comes back as a nil error with a non-zero
// ExitCode, so callers can print its output before reporting the failure.
type NodeExecutor interface {
	Exec(ctx context.Context, argv []string, timeoutSec int) (NodeExecResult, error)
	// ExecScript runs a script on the node, delivering its bytes as data
	// rather than as part of any command line (see nodescript.go). The error
	// and ExitCode follow the same contract as Exec.
	ExecScript(ctx context.Context, s NodeScript, timeoutSec int) (NodeExecResult, error)
}

// NewNodeExecutor dials the transport named by t.
func NewNodeExecutor(cfg *config.Config, t NodeTarget) (NodeExecutor, error) {
	switch t.Transport {
	case NodeTransportSSH:
		if t.SSH.Host == "" {
			return nil, fmt.Errorf("node %s: no ssh host recorded in state", t.Name)
		}
		if t.SSH.User == "" {
			return nil, fmt.Errorf("node %s: no ssh_user for its pool in state or config", t.Name)
		}
		return &sshExecutor{target: t.SSH, os: t.OS}, nil
	case NodeTransportHypervisor:
		prov, err := newProvider(cfg, t.Provider)
		if err != nil {
			return nil, fmt.Errorf("node %s: %s backend: %w", t.Name, t.Provider, err)
		}
		return &hypervisorExecutor{prov: prov, node: t.Node, id: t.ID, name: t.Name, os: t.OS}, nil
	default:
		return nil, fmt.Errorf("node %s: unknown transport %q", t.Name, t.Transport)
	}
}

// NodeExecutorFor is the one-call path the CLI uses: resolve, then dial.
func NodeExecutorFor(cfg *config.Config, pool *state.Pool, member *state.Member) (NodeTarget, NodeExecutor, error) {
	t := ResolveNodeTarget(cfg, pool, member)
	ex, err := NewNodeExecutor(cfg, t)
	return t, ex, err
}

// hypervisorExecutor runs commands through provider.Instance.Exec — the same
// call the apply path makes, so node commands and apply share one code path.
type hypervisorExecutor struct {
	prov provider.Provider
	node string
	id   int
	name string
	os   string
	// inst is resolved on first use and reused: `node upgrade` execs twice
	// (version probe, then the upgrade).
	inst provider.Instance
}

func (h *hypervisorExecutor) Exec(ctx context.Context, argv []string, timeoutSec int) (NodeExecResult, error) {
	if err := h.instance(ctx); err != nil {
		return NodeExecResult{}, err
	}
	st, err := h.inst.Exec(ctx, argv, timeoutSec)
	if err != nil {
		return NodeExecResult{}, err
	}
	return NodeExecResult{Stdout: st.OutData, Stderr: st.ErrData, ExitCode: st.ExitCode}, nil
}

// instance resolves h.inst on first use.
func (h *hypervisorExecutor) instance(ctx context.Context) error {
	if h.inst != nil {
		return nil
	}
	inst, err := h.prov.GetInstance(ctx, h.node, h.id)
	if err != nil {
		return fmt.Errorf("look up instance %d for %s: %w", h.id, h.name, err)
	}
	h.inst = inst
	return nil
}

// sshExecutor runs argv over the shared SSH transport (which wraps it in
// `sudo sh -c`).
//
// Two asymmetries with the hypervisor channel, both inherent to SSHRun and both
// benign for these commands:
//
//   - stdout and stderr come back merged. The system ssh path cannot separate
//     them, so everything lands in Stdout and Stderr stays empty.
//   - the remote exit status IS recovered. ssh exits with the remote status and
//     the embedded client reports it directly, so a failing command still
//     reaches the caller as a real exit code rather than an opaque error.
type sshExecutor struct {
	target SSHTarget
	os     string
}

// sshRun is the SSH transport. A var so tests can capture exactly what would
// cross the wire.
var sshRun = SSHRun

func (s *sshExecutor) Exec(ctx context.Context, argv []string, timeoutSec int) (NodeExecResult, error) {
	return s.run(ctx, ShellJoin(argv), nil, timeoutSec)
}

// run sends one command line, plus optional stdin, and maps the outcome onto
// NodeExecResult.
func (s *sshExecutor) run(ctx context.Context, command string, stdin []byte, timeoutSec int) (NodeExecResult, error) {
	if timeoutSec > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, time.Duration(timeoutSec)*time.Second)
		defer cancel()
	}
	out, err := sshRun(ctx, s.target, command, stdin)
	if err != nil {
		if code, ok := remoteExitCode(err); ok {
			return NodeExecResult{Stdout: out, ExitCode: code}, nil
		}
		return NodeExecResult{Stdout: out}, err
	}
	return NodeExecResult{Stdout: out}, nil
}

// remoteExitCode digs the remote command's exit status out of an SSHRun error,
// reporting false when the failure was not the command's own.
//
// The system-ssh path needs one carve-out: ssh reports its OWN failures
// (unreachable host, auth refused, bad key) as exit 255. Reporting that as
// "your command exited 255" would turn every unreachable node into a
// plausible-looking command failure, so 255 stays an error. A remote command
// that genuinely exits 255 is misreported as unreachable — rare, and the
// error text carries ssh's own diagnostics, which say which it was.
func remoteExitCode(err error) (int, bool) {
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		if ee.ExitCode() == sshSelfFailureCode {
			return 0, false
		}
		return ee.ExitCode(), true
	}
	// The embedded (password-auth) client reports connection failures as
	// distinct error types, so an ExitError here is always the remote command's.
	var se *gossh.ExitError
	if errors.As(err, &se) {
		return se.ExitStatus(), true
	}
	return 0, false
}

// sshSelfFailureCode is the exit status ssh(1) uses for its own errors.
const sshSelfFailureCode = 255

// ShellJoin quotes argv into a single POSIX sh command line. Exported because
// the SSH transport takes a script, not an argv, and callers building one by
// hand must quote the same way.
func ShellJoin(argv []string) string {
	quoted := make([]string, len(argv))
	for i, a := range argv {
		quoted[i] = "'" + strings.ReplaceAll(a, "'", `'\''`) + "'"
	}
	return strings.Join(quoted, " ")
}

// ResolveServiceVMTarget resolves the mayfly-managed VMs that are not pool
// members — the registry-cache VM, the model server and the monitoring host — so `node exec` can
// reach them through the same guest-agent channel as a runner. They are
// matched by their recorded hostname or by role ("registry-cache", "model-server",
// "monitoring"). All are Linux Proxmox VMs. Pure, like
// ResolveNodeTarget.
func ResolveServiceVMTarget(st *state.Store, name string) (NodeTarget, bool) {
	vm := func(host string, node string, id int) NodeTarget {
		return NodeTarget{
			Transport: NodeTransportHypervisor, Provider: config.ProviderProxmox,
			Node: node, ID: id, Name: host, OS: "linux",
		}
	}
	if rc := st.RegistryCache; rc != nil && rc.VMID != 0 && (name == rc.Hostname || name == "registry-cache") {
		return vm(rc.Hostname, rc.PVENode, rc.VMID), true
	}
	if ms := st.ModelServer; ms != nil && ms.VMID != 0 && (name == ms.Hostname || name == "model-server") {
		return vm(ms.Hostname, ms.PVENode, ms.VMID), true
	}
	if m := st.Monitoring; m != nil && m.VMID != 0 && (name == m.Hostname || name == "monitoring") {
		return vm(m.Hostname, m.PVENode, m.VMID), true
	}
	return NodeTarget{}, false
}
