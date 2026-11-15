package engine

// Provider-backed node lifecycle operations for `mayfly node resize|reboot|power`.
//
// Unlike status/exec/drain/upgrade (which only need to run a command *inside* a
// node and so route through NodeExecutor), these commands drive the VM's own
// lifecycle — CPU/memory/disk and power state — which lives on the hypervisor,
// not in the guest. They therefore need a provider.Instance, and they only work
// on hypervisor-backed nodes: an SSH-enrolled Mac or bare-metal box has no
// hypervisor handle to resize or power-cycle at all.
//
// The sequences here mirror the apply path's own power-cycle (see
// updateMember): best-effort in-guest drain, graceful shutdown, mutate, start,
// wait for the guest agent. They are written against provider.Instance so the
// ordering (shutdown before start, shrink refusal) is unit-testable against a
// fake instance without a live hypervisor.

import (
	"context"
	"errors"
	"fmt"

	"github.com/luthermonson/mayfly/internal/config"
	"github.com/luthermonson/mayfly/internal/provider"
	"github.com/luthermonson/mayfly/internal/state"
)

// ErrNoInstance means a node has no hypervisor handle — it is reached over SSH
// (darwin or enrolled bare metal), so it has no VM to resize or power-cycle.
var ErrNoInstance = errors.New("node has no hypervisor instance (reached over SSH)")

// nodePowerReadyTimeoutSec bounds the wait for the guest agent to come back
// after a power-on, matching the apply path's post-cycle wait (updateMember).
const nodePowerReadyTimeoutSec = 180

// nodeDrainTimeoutSec bounds the best-effort in-guest `ephemerd drain` before a
// graceful power operation, matching drainAndShutdown.
const nodeDrainTimeoutSec = 330

// NodeInstanceFor resolves the provider.Instance backing a node so operator
// commands can drive its lifecycle (resize, power) directly. It is the
// power/resize analogue of NodeExecutorFor. A node that ResolveNodeTarget routes
// to SSH has no hypervisor instance and returns ErrNoInstance; the routing
// decision itself is the pure, table-tested ResolveNodeTarget.
func NodeInstanceFor(ctx context.Context, cfg *config.Config, pool *state.Pool, member *state.Member) (NodeTarget, provider.Instance, error) {
	t := ResolveNodeTarget(cfg, pool, member)
	if t.Transport != NodeTransportHypervisor {
		return t, nil, fmt.Errorf("%w: node %s is reached over %s", ErrNoInstance, t.Name, t.Transport)
	}
	prov, err := newProvider(cfg, t.Provider)
	if err != nil {
		return t, nil, fmt.Errorf("node %s: %s backend: %w", t.Name, t.Provider, err)
	}
	inst, err := prov.GetInstance(ctx, t.Node, t.ID)
	if err != nil {
		return t, nil, fmt.Errorf("node %s: look up instance %d on %s: %w", t.Name, t.ID, t.Node, err)
	}
	return t, inst, nil
}

// ResizeSpec is a fully-resolved node resize. Cores/MemoryMB are the *target*
// values — the caller fills any flag the operator did not set from current
// state, because Proxmox writes both in a single config call — and the *Set
// booleans say which of the two operations (resources, disk) to actually run.
type ResizeSpec struct {
	Cores        int
	MemoryMB     int
	SetResources bool

	DiskGB     int
	ResizeDisk bool
}

// Empty reports whether the spec asks for nothing.
func (r ResizeSpec) Empty() bool { return !r.SetResources && !r.ResizeDisk }

// ResizeApplied reports which hypervisor changes ResizeNode actually landed, so
// the caller can persist them to state even when a LATER step (start, guest-agent
// wait, filesystem grow, serving check) fails. The VM keeps the new resources
// regardless of those later failures, so state must record them or it goes stale.
type ResizeApplied struct {
	Cores, MemoryMB  int
	ResourcesApplied bool
	DiskGB           int
	DiskApplied      bool
}

// logfFn is a minimal progress sink so these ops can report without depending
// on the Engine. nil is tolerated.
type logfFn func(format string, args ...any)

func (l logfFn) logf(format string, args ...any) {
	if l != nil {
		l(format, args...)
	}
}

// ResizeNode power-cycles a single hypervisor node to apply new resources:
// best-effort in-guest drain, graceful shutdown, apply cores/memory and/or grow
// the disk, start, wait for the guest agent, and confirm ephemerd is serving.
// Only the operations flagged in req run.
//
// A disk request smaller than currentDiskGB is refused outright, before any
// power change: Proxmox cannot shrink a disk, and issuing the resize would at
// best no-op and at worst corrupt the filesystem. currentDiskGB <= 0 (unknown)
// disables the guard.
func ResizeNode(ctx context.Context, inst provider.Instance, osName string, req ResizeSpec, currentDiskGB int, logf func(string, ...any)) (ResizeApplied, error) {
	log := logfFn(logf)
	var applied ResizeApplied
	if req.Empty() {
		return applied, fmt.Errorf("resize: nothing to do; set at least one of --cores, --memory-mb, --disk-gb")
	}
	if req.ResizeDisk && currentDiskGB > 0 && req.DiskGB < currentDiskGB {
		return applied, fmt.Errorf("resize: refusing to shrink disk from %dGB to %dGB — Proxmox cannot shrink a disk", currentDiskGB, req.DiskGB)
	}

	if err := drainBeforePowerOff(ctx, inst, osName, false, log); err != nil {
		return applied, err
	}
	if err := inst.Shutdown(ctx); err != nil {
		return applied, fmt.Errorf("shutdown: %w", err)
	}

	// Record each hypervisor change into `applied` the instant it succeeds, so a
	// failure of any later step still lets the caller persist what the VM now
	// actually has. The VM keeps these settings across a failed start/wait.
	if req.SetResources {
		log.logf("setting resources to %d cores / %d MB", req.Cores, req.MemoryMB)
		if err := inst.SetResources(ctx, req.Cores, req.MemoryMB); err != nil {
			return applied, fmt.Errorf("set resources: %w", err)
		}
		applied.Cores, applied.MemoryMB, applied.ResourcesApplied = req.Cores, req.MemoryMB, true
	}
	if req.ResizeDisk {
		log.logf("resizing disk to %dGB", req.DiskGB)
		if err := inst.ResizeDisk(ctx, req.DiskGB); err != nil {
			return applied, fmt.Errorf("resize disk: %w", err)
		}
		applied.DiskGB, applied.DiskApplied = req.DiskGB, true
	}

	log.logf("starting")
	if err := inst.Start(ctx); err != nil {
		return applied, fmt.Errorf("start: %w", err)
	}
	if err := inst.WaitForReady(ctx, nodePowerReadyTimeoutSec); err != nil {
		return applied, fmt.Errorf("guest agent after resize: %w", err)
	}
	// A disk grow needs the guest to expand the filesystem into the new space;
	// the config write alone only enlarges the virtual disk.
	if req.ResizeDisk {
		log.logf("growing filesystem")
		if st, err := inst.Exec(ctx, growCmd(osName), 120); err != nil {
			log.logf("filesystem grow could not be confirmed (%v); the disk is enlarged but may need a manual grow", err)
		} else if st.ExitCode != 0 {
			log.logf("filesystem grow exited %d: %s", st.ExitCode, firstLine(st.ErrData))
		}
	}
	return applied, verifyServing(ctx, inst, osName, log)
}

// RebootNode gracefully reboots a hypervisor node: best-effort in-guest drain,
// graceful shutdown (the provider owns the ACPI→hard fallback), start, wait for
// the guest agent, and confirm ephemerd is serving. This is the polite reboot;
// `node power reset` is the hard one for a hung guest.
//
// The ordering is the safety property, and it rests on the provider: Shutdown
// returns only once the hypervisor reports the VM stopped (escalating to a
// hard stop when the graceful attempt fails, which it does when the guest
// agent is dead), and Start returns only once the VM is running and stays
// running. Before that, a failed shutdown read as success, the start raced a
// VM that was still powering down, and the node was left stopped.
func RebootNode(ctx context.Context, inst provider.Instance, osName string, opts RebootOptions, logf func(string, ...any)) error {
	log := logfFn(logf)
	if err := drainBeforePowerOff(ctx, inst, osName, opts.RequireDrain, log); err != nil {
		return err
	}
	log.logf("shutting down (graceful; escalates to a hard stop if the VM does not stop)")
	if err := inst.Shutdown(ctx); err != nil {
		return fmt.Errorf("shutdown: %w", err)
	}
	log.logf("stopped; starting")
	if err := inst.Start(ctx); err != nil {
		return fmt.Errorf("start: %w", err)
	}
	log.logf("running; waiting for guest agent")
	if err := inst.WaitForReady(ctx, nodePowerReadyTimeoutSec); err != nil {
		return fmt.Errorf("guest agent after reboot: %w", err)
	}
	return verifyServing(ctx, inst, osName, log)
}

// RebootOptions tunes RebootNode.
type RebootOptions struct {
	// RequireDrain refuses to power the node off when it cannot be drained
	// (guest agent unreachable, or the drain itself failed), instead of
	// rebooting through whatever jobs are running.
	RequireDrain bool
}

// drainBeforePowerOff runs the best-effort in-guest `ephemerd drain` ahead of a
// power-off and says plainly what a failure means. With the guest agent dead
// there is no channel to drain over at all, so any running job will be killed
// by the power-off. That used to surface as "drain did not complete cleanly
// (no pid returned from agent exec command)", which does not tell an operator
// that jobs are about to die. requireDrain turns either failure into a refusal.
func drainBeforePowerOff(ctx context.Context, inst provider.Instance, osName string, requireDrain bool, log logfFn) error {
	log.logf("draining")
	st, err := inst.Exec(ctx, drainCmd(osName), nodeDrainTimeoutSec)
	var problem string
	switch {
	case err != nil:
		problem = fmt.Sprintf("guest agent unreachable (%v): cannot drain", err)
	case st.ExitCode != 0:
		problem = fmt.Sprintf("drain exited %d (%s)", st.ExitCode, firstLine(st.ErrData+st.OutData))
	default:
		return nil
	}
	if requireDrain {
		return fmt.Errorf("%s; refusing to power off because --require-drain is set "+
			"(running jobs would be interrupted; drop the flag, or use `mayfly node power reset` to force it)", problem)
	}
	log.logf("%s; running jobs will be interrupted", problem)
	return nil
}

// PowerAction is a hard, guest-agent-independent power operation.
type PowerAction string

const (
	// PowerStop hard-powers the node off (no graceful ACPI). Recovers a hung
	// guest whose OS or agent is not responding.
	PowerStop PowerAction = "stop"
	// PowerStart powers the node on.
	PowerStart PowerAction = "start"
	// PowerReset hard-cycles the node: Stop then Start.
	PowerReset PowerAction = "reset"
)

// PowerNode performs a hard power operation straight against the hypervisor,
// bypassing the guest agent entirely — the recovery path when a guest agent has
// wedged and the graceful reboot cannot drain or shut down. It does not wait for
// the guest agent (a hung agent is exactly why an operator reaches for this);
// verify with `mayfly node status` once it settles.
func PowerNode(ctx context.Context, inst provider.Instance, action PowerAction, logf func(string, ...any)) error {
	log := logfFn(logf)
	switch action {
	case PowerStop:
		log.logf("hard power-off")
		return inst.Stop(ctx)
	case PowerStart:
		log.logf("power-on")
		return inst.Start(ctx)
	case PowerReset:
		log.logf("hard reset (stop, then start)")
		if err := inst.Stop(ctx); err != nil {
			return fmt.Errorf("stop: %w", err)
		}
		log.logf("power-on")
		return inst.Start(ctx)
	default:
		return fmt.Errorf("unknown power action %q (want stop, start, or reset)", action)
	}
}

// verifyServing confirms ephemerd answers a status probe after a power-on, so a
// resize/reboot that leaves the daemon dead is reported rather than declared a
// success. Best-effort: it turns a non-serving daemon into an error but does not
// parse the status body.
func verifyServing(ctx context.Context, inst provider.Instance, osName string, log logfFn) error {
	st, err := inst.Exec(ctx, statusCmd(osName), 60)
	if err != nil {
		return fmt.Errorf("verify serving: %w", err)
	}
	if st.ExitCode != 0 {
		return fmt.Errorf("node is up but ephemerd status exited %d: %s", st.ExitCode, firstLine(st.ErrData))
	}
	log.logf("up and serving")
	return nil
}
