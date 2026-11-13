package provider

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/luthermonson/go-proxmox"
)

// qgaTimeout is the error PVE returns when qemu-guest-agent does not answer in
// time. It is what mayfly saw for both Windows ephemerd upgrades on
// 2026-08-16, in each case while the upgrade was succeeding.
var qgaTimeout = errors.New("500 VM 101 qga command 'guest-exec-status' failed - got timeout")

func fastPolling(t *testing.T, poll, grace time.Duration) {
	t.Helper()
	prevMin, prevMax, prevGrace := agentExecStatusPollMin, agentExecStatusPollMax, agentExecStatusGrace
	agentExecStatusPollMin, agentExecStatusPollMax, agentExecStatusGrace = poll, poll, grace
	t.Cleanup(func() {
		agentExecStatusPollMin, agentExecStatusPollMax, agentExecStatusGrace = prevMin, prevMax, prevGrace
	})
}

// scriptedStatus replays one guest-exec-status answer per call, repeating the
// last forever.
func scriptedStatus(steps ...func() (*proxmox.AgentExecStatus, error)) (agentExecStatusFn, *int) {
	calls := new(int)
	return func(context.Context) (*proxmox.AgentExecStatus, error) {
		i := *calls
		if i >= len(steps) {
			i = len(steps) - 1
		}
		*calls++
		return steps[i]()
	}, calls
}

func running() (*proxmox.AgentExecStatus, error) {
	return &proxmox.AgentExecStatus{Exited: 0}, nil
}

func exited(code int) func() (*proxmox.AgentExecStatus, error) {
	return func() (*proxmox.AgentExecStatus, error) {
		return &proxmox.AgentExecStatus{Exited: 1, ExitCode: code, OutData: "done"}, nil
	}
}

func failing() (*proxmox.AgentExecStatus, error) { return nil, qgaTimeout }

// TestWaitForAgentExec_RidesOutAgentGap is the regression. A command that
// restarts the guest's services makes the guest agent unreachable for a few
// seconds; the old loop aborted on the first such error and reported the
// command as failed. It must ride the gap out instead.
func TestWaitForAgentExec_RidesOutAgentGap(t *testing.T) {
	fastPolling(t, time.Millisecond, time.Second)
	poll, calls := scriptedStatus(running, failing, failing, failing, exited(0))

	st, err := waitForAgentExec(context.Background(), poll, 4242, 10*time.Second)
	if err != nil {
		t.Fatalf("waitForAgentExec gave up on a transient agent gap: %v", err)
	}
	if st.ExitCode != 0 || st.OutData != "done" {
		t.Errorf("status = %+v, want the real exit", st)
	}
	if *calls < 5 {
		t.Errorf("polled %d times, want it to have kept trying through the gap", *calls)
	}
}

// A gap that never closes still has to end, and as a distinguishable error:
// the guest process is untouched, so "I lost sight of it" is the truth, not
// "it failed".
func TestWaitForAgentExec_GivesUpAfterGrace(t *testing.T) {
	fastPolling(t, time.Millisecond, 20*time.Millisecond)
	poll, _ := scriptedStatus(running, failing)

	_, err := waitForAgentExec(context.Background(), poll, 4242, 10*time.Second)
	if err == nil {
		t.Fatal("want an error once the grace period expires")
	}
	if !errors.Is(err, ErrExecStatusLost) {
		t.Errorf("error = %v, want it to wrap ErrExecStatusLost", err)
	}
	if !errors.Is(err, qgaTimeout) {
		t.Errorf("error = %v, want it to carry the agent's own error", err)
	}
}

// The caller's timeout is also "lost sight of", not failure: nothing kills the
// guest process when it expires.
func TestWaitForAgentExec_TimeoutIsLostNotFailed(t *testing.T) {
	fastPolling(t, time.Millisecond, time.Minute)
	poll, _ := scriptedStatus(running)

	_, err := waitForAgentExec(context.Background(), poll, 4242, 15*time.Millisecond)
	if !errors.Is(err, ErrExecStatusLost) {
		t.Fatalf("error = %v, want ErrExecStatusLost", err)
	}
}

func TestWaitForAgentExec_ReturnsExitCode(t *testing.T) {
	fastPolling(t, time.Millisecond, time.Minute)
	poll, _ := scriptedStatus(running, exited(3))

	st, err := waitForAgentExec(context.Background(), poll, 4242, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if st.ExitCode != 3 {
		t.Errorf("ExitCode = %d, want 3", st.ExitCode)
	}
}

// PVE can answer the exec-status call with no result body, which upstream
// turns into (nil, nil). Dereferencing that would panic; it belongs in the
// same "can't see the command" bucket as an unreachable agent.
func TestWaitForAgentExec_EmptyStatusIsNotAPanic(t *testing.T) {
	fastPolling(t, time.Millisecond, 20*time.Millisecond)
	empty := func() (*proxmox.AgentExecStatus, error) { return nil, nil }
	poll, _ := scriptedStatus(running, empty)

	_, err := waitForAgentExec(context.Background(), poll, 4242, 10*time.Second)
	if !errors.Is(err, ErrExecStatusLost) {
		t.Fatalf("error = %v, want ErrExecStatusLost", err)
	}
}

// The ramp is what keeps short commands short. A command that has already
// finished must come back on roughly the first poll, not after a full
// steady-state tick.
func TestWaitForAgentExec_ShortCommandIsNotTaxedByTheSlowTick(t *testing.T) {
	prevMin, prevMax := agentExecStatusPollMin, agentExecStatusPollMax
	agentExecStatusPollMin, agentExecStatusPollMax = 5*time.Millisecond, 2*time.Second
	t.Cleanup(func() { agentExecStatusPollMin, agentExecStatusPollMax = prevMin, prevMax })

	poll, _ := scriptedStatus(running, exited(0))
	start := time.Now()
	if _, err := waitForAgentExec(context.Background(), poll, 4242, time.Minute); err != nil {
		t.Fatal(err)
	}
	// Two polls one min-interval apart, not one max-interval apart.
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("took %s for a command that finished immediately; the ramp is not starting fast", elapsed)
	}
}

func TestWaitForAgentExec_HonorsContextCancellation(t *testing.T) {
	fastPolling(t, 5*time.Millisecond, time.Minute)
	ctx, cancel := context.WithCancel(context.Background())
	poll, _ := scriptedStatus(running)
	go func() {
		time.Sleep(10 * time.Millisecond)
		cancel()
	}()

	if _, err := waitForAgentExec(ctx, poll, 4242, time.Minute); !errors.Is(err, context.Canceled) {
		t.Errorf("error = %v, want context.Canceled", err)
	}
}
