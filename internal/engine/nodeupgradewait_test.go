package engine

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

// scriptedExec replays one canned response per Exec call (repeating the last
// one forever), which is how a node behaves across an upgrade: answering, then
// unreachable while the service restarts, then answering again.
type scriptedExec struct {
	steps []execStep
	calls int
	argv  [][]string
}

type execStep struct {
	stdout string
	code   int
	err    error
}

func (s *scriptedExec) Exec(_ context.Context, argv []string, _ int) (NodeExecResult, error) {
	s.argv = append(s.argv, argv)
	i := s.calls
	if i >= len(s.steps) {
		i = len(s.steps) - 1
	}
	s.calls++
	st := s.steps[i]
	if st.err != nil {
		return NodeExecResult{}, st.err
	}
	return NodeExecResult{Stdout: st.stdout, ExitCode: st.code}, nil
}

func (s *scriptedExec) ExecScript(context.Context, NodeScript, int) (NodeExecResult, error) {
	return NodeExecResult{}, errors.New("scriptedExec: the upgrade wait runs no scripts")
}

// statusJSON mirrors what `ephemerd status` actually prints: pretty-printed
// with MarshalIndent, keys in the daemon's order. Verified against
// mfl-win-amd64-101 on 2026-08-17.
func statusJSON(version string, draining bool, uptime string) string {
	return statusJSONJobs(version, draining, uptime, 0)
}

func statusJSONJobs(version string, draining bool, uptime string, active int) string {
	return fmt.Sprintf(`{
  "active_jobs": %d,
  "draining": %t,
  "max_concurrent": 2,
  "status": "ok",
  "uptime": %q,
  "version": %q
}`, active, draining, uptime, version)
}

func shortPoll(t *testing.T) {
	t.Helper()
	prev := upgradeVerifyPoll
	upgradeVerifyPoll = time.Millisecond
	t.Cleanup(func() { upgradeVerifyPoll = prev })
}

func TestParseNodeHealth(t *testing.T) {
	t.Run("plain json", func(t *testing.T) {
		h, err := parseNodeHealth(statusJSON("v0.2.3", false, "12h48m48s"))
		if err != nil {
			t.Fatal(err)
		}
		if h.Version != "v0.2.3" || h.Draining || h.Uptime != "12h48m48s" {
			t.Errorf("got %+v", h)
		}
	})

	// Not the path EphemerdArgv uses — it execs the daemon directly, with no
	// shell — but SSH merges stderr into stdout and a `powershell -Command`
	// wrapper adds a CLIXML preamble, so the parse stays defensive.
	t.Run("surrounding noise", func(t *testing.T) {
		noisy := "#< CLIXML\n<Objs Version=\"1.1.0.1\"><Obj S=\"progress\"/></Objs>\n" +
			statusJSON("v0.2.3", true, "6h54m26s") + "\n"
		h, err := parseNodeHealth(noisy)
		if err != nil {
			t.Fatal(err)
		}
		if h.Version != "v0.2.3" || !h.Draining {
			t.Errorf("got %+v, want v0.2.3 draining", h)
		}
	})

	t.Run("no json at all", func(t *testing.T) {
		if _, err := parseNodeHealth("dial unix: connection refused"); err == nil {
			t.Fatal("want an error when there is no status JSON")
		}
	})
}

func TestSameVersion(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"v0.2.3", "v0.2.3", true},
		{"ephemerd version v0.2.3", "v0.2.3", true},
		{"0.2.3", "v0.2.3", true},
		{"v0.2.2", "v0.2.3", false},
		{"", "v0.2.3", false},
		{"v0.2.3", "", false},
	}
	for _, c := range cases {
		if got := sameVersion(c.a, c.b); got != c.want {
			t.Errorf("sameVersion(%q, %q) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
}

// TestAwaitUpgrade_SucceedsAfterRestartGap is the 2026-08-16 case: the node
// goes silent while the service restarts, then comes back on the new version.
// That whole sequence is a SUCCESS and must be reported as one.
func TestAwaitUpgrade_SucceedsAfterRestartGap(t *testing.T) {
	shortPoll(t)
	qgaGap := errors.New("500 VM 101 qga command 'guest-exec-status' failed - got timeout")
	ex := &scriptedExec{steps: []execStep{
		// The exact snapshot the operator caught on 2026-08-16: cordoned,
		// still on the old binary, one second before the swap landed.
		{stdout: statusJSON("v0.2.2", true, "6h54m26s")},
		{err: qgaGap}, // service restarting; the guest agent goes quiet
		{err: qgaGap},
		{stdout: statusJSON("v0.2.3", false, "3s")}, // back on the new version
	}}

	v, err := AwaitUpgrade(context.Background(), ex, "windows", "v0.2.3", 10*time.Second, nil)
	if err != nil {
		t.Fatalf("AwaitUpgrade: %v", err)
	}
	if !v.Done() {
		t.Fatalf("verdict = %+v, want done", v)
	}
	if v.Health.Version != "v0.2.3" || v.Health.Draining {
		t.Errorf("health = %+v", v.Health)
	}
}

// TestAwaitUpgrade_StillOldVersion is the inverse: the exec may well have
// exited 0, but the node never moved. This must be a failure.
func TestAwaitUpgrade_StillOldVersion(t *testing.T) {
	shortPoll(t)
	ex := &scriptedExec{steps: []execStep{{stdout: statusJSON("v0.2.2", false, "7h")}}}

	v, err := AwaitUpgrade(context.Background(), ex, "windows", "v0.2.3", 20*time.Millisecond, nil)
	if err != nil {
		t.Fatal(err)
	}
	if v.Done() {
		t.Fatal("verdict says done, but the node never left v0.2.2")
	}
	if !v.Answered || v.Reached {
		t.Errorf("verdict = %+v, want answered but not reached", v)
	}
	msg := DescribeUpgradeFailure(v, "v0.2.3", time.Minute, winExe).Error()
	if !strings.Contains(msg, "DID NOT HAPPEN") || !strings.Contains(msg, "v0.2.2") {
		t.Errorf("message = %q", msg)
	}
}

// TestAwaitUpgrade_UpgradedButStillDrained: right binary, still cordoned.
//
// This is cheap insurance rather than an expected outcome, and it is worth
// being clear about why: a node reporting the NEW version has necessarily
// restarted, and `draining` lives only in the scheduler's memory, so a fresh
// daemon always starts un-cordoned. Reaching this state means something
// cordoned the node AFTER the upgrade landed — a concurrent `ephemerd drain`,
// or an upgrade that was a no-op on a node that was already drained. Rare, but
// it is the one shape where the node looks healthy and takes no work, so it
// gets its own message instead of being folded into a generic failure.
func TestAwaitUpgrade_UpgradedButStillDrained(t *testing.T) {
	shortPoll(t)
	ex := &scriptedExec{steps: []execStep{{stdout: statusJSON("v0.2.3", true, "4m")}}}

	v, err := AwaitUpgrade(context.Background(), ex, "windows", "v0.2.3", 20*time.Millisecond, nil)
	if err != nil {
		t.Fatal(err)
	}
	if v.Done() {
		t.Fatal("a drained node must not count as a finished upgrade")
	}
	if !v.Reached || v.Serving {
		t.Errorf("verdict = %+v, want reached but not serving", v)
	}
	msg := DescribeUpgradeFailure(v, "v0.2.3", time.Minute, winExe).Error()
	if !strings.Contains(msg, "INCOMPLETE") || !strings.Contains(msg, "uncordon") {
		t.Errorf("message = %q, want it to flag the cordon and name the fix", msg)
	}
}

// TestAwaitUpgrade_GoesSilentIsUnverifiedNotFailed is the exact shape of this
// fleet's known Windows failure: the swap lands, the service never comes back.
// The last thing we saw was the legitimate mid-upgrade cordon, and describing
// the node from that stale sample produced a verdict in which every clause was
// false at the moment it printed. Losing sight of a node is UNVERIFIED.
func TestAwaitUpgrade_GoesSilentIsUnverifiedNotFailed(t *testing.T) {
	shortPoll(t)
	gone := errors.New("dial unix ephemerd.sock: connection refused")
	ex := &scriptedExec{steps: []execStep{
		{stdout: statusJSON("v0.2.2", true, "6h54m26s")}, // mid-upgrade snapshot
		{err: gone}, // ...and then nothing, ever
	}}

	v, err := AwaitUpgrade(context.Background(), ex, "windows", "v0.2.3", 30*time.Millisecond, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !v.Answered {
		t.Error("Answered should stay true; it did answer once")
	}
	if v.Fresh {
		t.Error("Fresh must be false once the final probe failed")
	}
	if v.Done() {
		t.Fatal("a node that went silent is not a finished upgrade")
	}

	msg := DescribeUpgradeFailure(v, "v0.2.3", time.Minute, winExe).Error()
	if !strings.Contains(msg, "UNVERIFIED") {
		t.Errorf("message = %q, want UNVERIFIED", msg)
	}
	if strings.Contains(msg, "DID NOT HAPPEN") {
		t.Errorf("message = %q, must not claim the upgrade did not happen — we cannot see the node", msg)
	}
	// The stale sample may be shown, but only labelled as stale.
	if strings.Contains(msg, "6h54m26s") && !strings.Contains(msg, "STALE") {
		t.Errorf("message = %q, want the stale snapshot labelled", msg)
	}
	if !strings.Contains(msg, "connection refused") {
		t.Errorf("message = %q, want the last probe error carried through", msg)
	}
}

// TestAwaitUpgrade_DrainingIsInProgressNotFailure: ephemerd drains running
// jobs BEFORE downloading, waiting up to 45m, and reports the old version with
// draining=true the whole time. mayfly's default budget is ~16m, so a node
// running a long job trips the deadline while perfectly healthy. Calling that
// "DID NOT HAPPEN ... check the log" is the same false failure this command
// exists to eliminate.
func TestAwaitUpgrade_DrainingIsInProgressNotFailure(t *testing.T) {
	shortPoll(t)
	ex := &scriptedExec{steps: []execStep{
		{stdout: statusJSONJobs("v0.2.2", true, "3h", 1)}, // cordoned, one job still running
	}}

	v, err := AwaitUpgrade(context.Background(), ex, "linux", "v0.2.3", 20*time.Millisecond, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !v.InFlight() {
		t.Fatalf("verdict = %+v, want InFlight", v)
	}
	msg := DescribeUpgradeFailure(v, "v0.2.3", time.Minute, unixExe).Error()
	if !strings.Contains(msg, "STILL IN PROGRESS") {
		t.Errorf("message = %q, want it to say the upgrade is still running", msg)
	}
	if strings.Contains(msg, "DID NOT HAPPEN") {
		t.Errorf("message = %q, must not call a healthy in-flight drain a failure", msg)
	}
	if !strings.Contains(msg, "1 running job") {
		t.Errorf("message = %q, want it to name what the drain is waiting on", msg)
	}
	if strings.Contains(msg, "why the upgrade or its restart failed") {
		t.Errorf("message = %q, must not send the operator to a log that says nothing is wrong", msg)
	}
}

func TestAwaitUpgrade_NeverAnswers(t *testing.T) {
	shortPoll(t)
	ex := &scriptedExec{steps: []execStep{{err: errors.New("no route to host")}}}

	v, err := AwaitUpgrade(context.Background(), ex, "linux", "v0.2.3", 20*time.Millisecond, nil)
	if err != nil {
		t.Fatal(err)
	}
	if v.Answered || v.Done() {
		t.Errorf("verdict = %+v, want unanswered", v)
	}
	msg := DescribeUpgradeFailure(v, "v0.2.3", time.Minute, unixExe).Error()
	if !strings.Contains(msg, "UNVERIFIED") {
		t.Errorf("message = %q", msg)
	}
}

// A binary_url-pinned install has no release tag to assert, so coming back and
// serving is the whole test.
func TestAwaitUpgrade_NoTargetVersionOnlyChecksServing(t *testing.T) {
	shortPoll(t)
	ex := &scriptedExec{steps: []execStep{{stdout: statusJSON("v0.1.10-5-gda802c9", false, "2s")}}}

	v, err := AwaitUpgrade(context.Background(), ex, "linux", "", time.Second, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !v.Done() {
		t.Fatalf("verdict = %+v, want done", v)
	}
}

func TestAwaitUpgrade_ProbesStatusOnTheRightPath(t *testing.T) {
	shortPoll(t)
	ex := &scriptedExec{steps: []execStep{{stdout: statusJSON("v0.2.3", false, "1s")}}}
	if _, err := AwaitUpgrade(context.Background(), ex, "windows", "v0.2.3", time.Second, nil); err != nil {
		t.Fatal(err)
	}
	if len(ex.argv) == 0 {
		t.Fatal("no command was run")
	}
	got := ex.argv[0]
	want := EphemerdArgv("windows", "status")
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("ran %v, want %v", got, want)
	}
}

func TestAwaitUpgrade_NonZeroStatusExitIsNotAnAnswer(t *testing.T) {
	shortPoll(t)
	ex := &scriptedExec{steps: []execStep{{stdout: "", code: 1}}}
	v, err := AwaitUpgrade(context.Background(), ex, "linux", "v0.2.3", 20*time.Millisecond, nil)
	if err != nil {
		t.Fatal(err)
	}
	if v.Answered {
		t.Errorf("verdict = %+v, want unanswered for a failed status probe", v)
	}
}
