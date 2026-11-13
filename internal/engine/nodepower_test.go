package engine

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/ephemerd/mayfly/internal/provider"
	"github.com/ephemerd/mayfly/internal/state"
)

// fakeInstance is a provider.Instance that records the order of the lifecycle
// calls made against it, so the resize/reboot/power sequences can be asserted
// without a live hypervisor. Exec returns a serving (exit 0) status by default
// so verifyServing passes.
type fakeInstance struct {
	calls    []string
	execArgv [][]string

	setCores, setMem, resizedTo int
	diskGB                      int
	movedTo                     string
	moveErr                     error
	resizeErr                   error
	diskStorage, diskLock       string

	execStatus provider.ExecStatus
	execErr    error

	waitErr error // WaitForReady returns this (simulates a post-resize failure)
}

func (f *fakeInstance) rec(name string) { f.calls = append(f.calls, name) }

func (f *fakeInstance) ID() int { return 100 }

func (f *fakeInstance) SetResources(_ context.Context, cores, memoryMB int) error {
	f.rec("SetResources")
	f.setCores, f.setMem = cores, memoryMB
	return nil
}

func (f *fakeInstance) ResizeDisk(_ context.Context, sizeGB int) error {
	f.rec("ResizeDisk")
	if f.resizeErr != nil {
		return f.resizeErr
	}
	f.resizedTo = sizeGB
	return nil
}

func (f *fakeInstance) DiskGB(context.Context) (int, error) { return f.diskGB, nil }

func (f *fakeInstance) MoveDisk(_ context.Context, storage string) error {
	f.rec("MoveDisk")
	if f.moveErr != nil {
		return f.moveErr
	}
	f.movedTo = storage
	return nil
}

func (f *fakeInstance) DiskStorage(context.Context) (string, string, error) {
	return f.diskStorage, f.diskLock, nil
}

func (f *fakeInstance) Start(context.Context) error    { f.rec("Start"); return nil }
func (f *fakeInstance) Shutdown(context.Context) error { f.rec("Shutdown"); return nil }
func (f *fakeInstance) Stop(context.Context) error     { f.rec("Stop"); return nil }

func (f *fakeInstance) IsRunning(context.Context) (bool, error) { return true, nil }

func (f *fakeInstance) WaitForReady(context.Context, int) error {
	f.rec("WaitForReady")
	return f.waitErr
}

func (f *fakeInstance) Exec(_ context.Context, argv []string, _ int) (provider.ExecStatus, error) {
	f.rec("Exec")
	f.execArgv = append(f.execArgv, argv)
	return f.execStatus, f.execErr
}

func (f *fakeInstance) FileWrite(context.Context, string, []byte) error { return nil }
func (f *fakeInstance) PrimaryIPv4(context.Context) (string, error)     { return "", nil }
func (f *fakeInstance) Delete(context.Context) error                    { return nil }

// indexOf returns the position of the first call named name, or -1.
func indexOf(calls []string, name string) int {
	for i, c := range calls {
		if c == name {
			return i
		}
	}
	return -1
}

// mustOrder asserts that call `before` happens strictly before call `after`.
func mustOrder(t *testing.T, calls []string, before, after string) {
	t.Helper()
	bi, ai := indexOf(calls, before), indexOf(calls, after)
	if bi < 0 {
		t.Fatalf("%q was never called; calls=%v", before, calls)
	}
	if ai < 0 {
		t.Fatalf("%q was never called; calls=%v", after, calls)
	}
	if bi >= ai {
		t.Fatalf("%q (at %d) must come before %q (at %d); calls=%v", before, bi, after, ai, calls)
	}
}

// NodeInstanceFor must refuse an SSH-enrolled node (no hypervisor handle) with
// ErrNoInstance, and it must do so from the pure routing decision alone —
// before dialing anything — so the refusal is reliable even with no backend
// reachable.
func TestNodeInstanceForSSHNodeHasNoInstance(t *testing.T) {
	cfg := fleetConfig()
	pool := &state.Pool{Name: "mac-arm64", SSHUser: "ephemerd", SSHKey: "~/.ssh/ephemerd"}
	member := &state.Member{Hostname: "192.0.2.78", OS: "darwin", Arch: "arm64"}

	target, inst, err := NodeInstanceFor(context.Background(), cfg, pool, member)
	if !errors.Is(err, ErrNoInstance) {
		t.Fatalf("err = %v, want ErrNoInstance", err)
	}
	if inst != nil {
		t.Errorf("inst = %v, want nil for an SSH node", inst)
	}
	if target.Transport != NodeTransportSSH {
		t.Errorf("Transport = %q, want ssh", target.Transport)
	}
}

// A resize with no dimensions set is a no-op the caller should be told about,
// not a silent success.
func TestResizeNodeRefusesEmptyRequest(t *testing.T) {
	f := &fakeInstance{}
	_, err := ResizeNode(context.Background(), f, "linux", ResizeSpec{}, 60, nil)
	if err == nil {
		t.Fatal("want an error for an empty resize request")
	}
	if len(f.calls) != 0 {
		t.Errorf("empty request touched the instance: %v", f.calls)
	}
}

// The disk shrink guard is the important safety property: Proxmox cannot shrink
// a disk, so a smaller --disk-gb must be refused BEFORE the node is touched, not
// discovered after it has already been drained and shut down.
func TestResizeNodeRefusesDiskShrink(t *testing.T) {
	f := &fakeInstance{}
	_, err := ResizeNode(context.Background(), f, "linux",
		ResizeSpec{DiskGB: 40, ResizeDisk: true}, 60, nil)
	if err == nil || !strings.Contains(err.Error(), "shrink") {
		t.Fatalf("err = %v, want a disk-shrink refusal", err)
	}
	if len(f.calls) != 0 {
		t.Errorf("shrink refusal still touched the instance: %v", f.calls)
	}
}

// The important state-consistency property: when a step AFTER the hypervisor
// change fails (here, the guest agent never comes back), ResizeNode still
// reports the resources it applied, so the caller can persist them. The VM kept
// the new resources, so state must too — otherwise it goes stale.
func TestResizeNodeReportsAppliedEvenWhenLaterStepFails(t *testing.T) {
	f := &fakeInstance{waitErr: errors.New("guest agent never returned")}
	spec := ResizeSpec{Cores: 8, MemoryMB: 16384, SetResources: true, DiskGB: 120, ResizeDisk: true}
	applied, err := ResizeNode(context.Background(), f, "linux", spec, 60, nil)
	if err == nil {
		t.Fatal("want an error when WaitForReady fails")
	}
	if !applied.ResourcesApplied || applied.Cores != 8 || applied.MemoryMB != 16384 {
		t.Errorf("applied resources = %+v, want them reported despite the later failure", applied)
	}
	if !applied.DiskApplied || applied.DiskGB != 120 {
		t.Errorf("applied disk = %+v, want it reported despite the later failure", applied)
	}
}

// A disk grow (equal is allowed too) proceeds, and the same recorded size with
// an unknown current size (0) is never treated as a shrink.
func TestResizeNodeAllowsGrowAndUnknownCurrent(t *testing.T) {
	for _, tc := range []struct{ current, want int }{
		{current: 60, want: 80}, // grow
		{current: 0, want: 40},  // unknown current disables the guard
	} {
		f := &fakeInstance{}
		_, err := ResizeNode(context.Background(), f, "linux",
			ResizeSpec{DiskGB: tc.want, ResizeDisk: true}, tc.current, nil)
		if err != nil {
			t.Fatalf("current=%d want=%d: unexpected error %v", tc.current, tc.want, err)
		}
		if f.resizedTo != tc.want {
			t.Errorf("resized to %d, want %d", f.resizedTo, tc.want)
		}
	}
}

// The resource resize must power-cycle in order: drain/shutdown, then apply the
// config write while stopped, then start and wait. Applying resources before
// shutdown, or starting before applying, would not take effect / would race.
func TestResizeNodeResourceOrdering(t *testing.T) {
	f := &fakeInstance{}
	spec := ResizeSpec{Cores: 8, MemoryMB: 16384, SetResources: true}
	applied, err := ResizeNode(context.Background(), f, "linux", spec, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !applied.ResourcesApplied || applied.Cores != 8 || applied.MemoryMB != 16384 {
		t.Errorf("applied = %+v, want cores 8 / mem 16384 applied", applied)
	}
	mustOrder(t, f.calls, "Shutdown", "SetResources")
	mustOrder(t, f.calls, "SetResources", "Start")
	mustOrder(t, f.calls, "Start", "WaitForReady")
	if f.setCores != 8 || f.setMem != 16384 {
		t.Errorf("SetResources(%d, %d), want (8, 16384)", f.setCores, f.setMem)
	}
	// No disk op requested → disk is never touched.
	if indexOf(f.calls, "ResizeDisk") >= 0 {
		t.Errorf("ResizeDisk was called without a disk request: %v", f.calls)
	}
}

// A disk grow resizes the virtual disk while stopped, then grows the filesystem
// from inside once the guest is back — the config write alone leaves the extra
// space unallocated.
func TestResizeNodeDiskGrowsThenExtendsFilesystem(t *testing.T) {
	f := &fakeInstance{}
	spec := ResizeSpec{DiskGB: 100, ResizeDisk: true}
	applied, err := ResizeNode(context.Background(), f, "linux", spec, 60, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !applied.DiskApplied || applied.DiskGB != 100 {
		t.Errorf("applied = %+v, want disk 100 applied", applied)
	}
	mustOrder(t, f.calls, "Shutdown", "ResizeDisk")
	mustOrder(t, f.calls, "ResizeDisk", "Start")

	// The filesystem grow must actually run the grow command (growpart/resize2fs),
	// not merely "some Exec" — verifyServing also execs, so assert on the argv and
	// that it lands after the guest agent is ready.
	want := growCmd("linux")
	growIdx := -1
	for i, argv := range f.execArgv {
		if equalCalls(argv, want) {
			growIdx = i
			break
		}
	}
	if growIdx < 0 {
		t.Fatalf("filesystem grow command %q never ran; exec'd: %v", want, f.execArgv)
	}
	// Every Exec is recorded in f.calls as "Exec"; the grow one is the
	// (growIdx+1)-th "Exec". Confirm it comes after WaitForReady.
	execSeen, growCallPos := 0, -1
	for i, c := range f.calls {
		if c == "Exec" {
			if execSeen == growIdx {
				growCallPos = i
				break
			}
			execSeen++
		}
	}
	if ready := indexOf(f.calls, "WaitForReady"); ready < 0 || growCallPos < ready {
		t.Errorf("filesystem grow ran at call %d, before WaitForReady at %d; calls=%v", growCallPos, ready, f.calls)
	}
}

// Reboot is graceful: it must shut down before starting. Starting a running VM,
// or starting before the shutdown completes, is the bug this guards.
func TestRebootNodeShutsDownBeforeStart(t *testing.T) {
	f := &fakeInstance{}
	if err := RebootNode(context.Background(), f, "linux", RebootOptions{}, nil); err != nil {
		t.Fatal(err)
	}
	mustOrder(t, f.calls, "Shutdown", "Start")
	mustOrder(t, f.calls, "Start", "WaitForReady")
}

// Power is the hard path: it goes straight to Stop/Start and never issues the
// graceful Shutdown or waits on the (possibly hung) guest agent.
func TestPowerNode(t *testing.T) {
	t.Run("stop is a hard stop only", func(t *testing.T) {
		f := &fakeInstance{}
		if err := PowerNode(context.Background(), f, PowerStop, nil); err != nil {
			t.Fatal(err)
		}
		if want := []string{"Stop"}; !equalCalls(f.calls, want) {
			t.Errorf("calls = %v, want %v", f.calls, want)
		}
	})
	t.Run("reset stops then starts", func(t *testing.T) {
		f := &fakeInstance{}
		if err := PowerNode(context.Background(), f, PowerReset, nil); err != nil {
			t.Fatal(err)
		}
		mustOrder(t, f.calls, "Stop", "Start")
		if indexOf(f.calls, "Shutdown") >= 0 {
			t.Errorf("reset used the graceful Shutdown; it must be a hard Stop: %v", f.calls)
		}
		if indexOf(f.calls, "WaitForReady") >= 0 {
			t.Errorf("power reset waited on the guest agent; it must not: %v", f.calls)
		}
	})
	t.Run("unknown action errors", func(t *testing.T) {
		f := &fakeInstance{}
		if err := PowerNode(context.Background(), f, PowerAction("bounce"), nil); err == nil {
			t.Fatal("want an error for an unknown power action")
		}
	})
}

func equalCalls(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

// With the guest agent dead there is nothing to drain over. The reboot still
// proceeds (that is usually why the operator is rebooting), but it must say
// plainly that running jobs will be killed, not "drain did not complete
// cleanly".
func TestRebootNodeDeadAgentSaysJobsWillBeInterrupted(t *testing.T) {
	f := &fakeInstance{execErr: errors.New("no pid returned from agent exec command")}
	var logs []string
	logf := func(format string, args ...any) { logs = append(logs, fmt.Sprintf(format, args...)) }

	// verifyServing's status probe fails too (same dead agent), which is the
	// right verdict; the property under test is the drain message and order.
	_ = RebootNode(context.Background(), f, "linux", RebootOptions{}, logf)

	joined := strings.Join(logs, "\n")
	if !strings.Contains(joined, "guest agent unreachable") || !strings.Contains(joined, "running jobs will be interrupted") {
		t.Errorf("reboot output does not warn that jobs will be interrupted:\n%s", joined)
	}
	mustOrder(t, f.calls, "Shutdown", "Start")
}

// --require-drain refuses instead, before anything is powered off.
func TestRebootNodeRequireDrainRefusesWithoutDrain(t *testing.T) {
	f := &fakeInstance{execErr: errors.New("no pid returned from agent exec command")}
	err := RebootNode(context.Background(), f, "linux", RebootOptions{RequireDrain: true}, nil)
	if err == nil || !strings.Contains(err.Error(), "--require-drain") {
		t.Fatalf("err = %v, want a --require-drain refusal", err)
	}
	if indexOf(f.calls, "Shutdown") >= 0 || indexOf(f.calls, "Stop") >= 0 {
		t.Errorf("refused reboot still powered the node off: %v", f.calls)
	}
}

// A drain that ran and failed is treated the same way under --require-drain.
func TestRebootNodeRequireDrainRefusesFailedDrain(t *testing.T) {
	f := &fakeInstance{execStatus: provider.ExecStatus{ExitCode: 1, ErrData: "daemon not running"}}
	if err := RebootNode(context.Background(), f, "linux", RebootOptions{RequireDrain: true}, nil); err == nil {
		t.Fatal("want a refusal when the drain exits non-zero")
	}
	if indexOf(f.calls, "Shutdown") >= 0 {
		t.Errorf("refused reboot still shut the node down: %v", f.calls)
	}
}
