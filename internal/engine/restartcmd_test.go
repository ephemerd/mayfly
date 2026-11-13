package engine

import (
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The Windows restart waits for the service to STOP before starting it again,
// and that wait has to outlast ephemerd's drain.
//
// Regression for 2026-09-21: the wait was 30s while a node with one live job
// took ~4 minutes to drain. The wait expired, every Start-Service attempt
// failed because a STOPPING service cannot be started, and the script threw
// with the node down — a slow drain turned into an outage, and `mayfly apply`
// reported "ephemerd failed to start after retries".
//
// ephemerd allows a drain up to 5 minutes, so anything at or under that is
// still a coin flip on a busy node.
func TestRestartCmdWindowsWaitsOutTheDrain(t *testing.T) {
	joined := strings.Join(restartCmd("windows"), " ")

	m := regexp.MustCompile(`WaitForStatus\('Stopped','(\d+):(\d+):(\d+)'\)`).FindStringSubmatch(joined)
	if m == nil {
		t.Fatalf("no WaitForStatus('Stopped',...) in the windows restart:\n%s", joined)
	}
	h, _ := strconv.Atoi(m[1])
	min, _ := strconv.Atoi(m[2])
	sec, _ := strconv.Atoi(m[3])
	got := time.Duration(h)*time.Hour + time.Duration(min)*time.Minute + time.Duration(sec)*time.Second

	// ephemerd's own drain ceiling. The wait must clear it, not merely match.
	const ephemerdDrainCeiling = 5 * time.Minute
	if got <= ephemerdDrainCeiling {
		t.Errorf("stop wait is %s, which does not outlast ephemerd's %s drain: a node draining a live job "+
			"will be left Stopped", got, ephemerdDrainCeiling)
	}
}

// Whatever else happens, the script must not finish with the service down.
// The retry loop is sized for the port-release race (seconds), so when it is
// exhausted there is still a final unconditional attempt before throwing:
// a node back up on its old config is drift to fix next apply, a node that
// never restarted is a dead runner.
func TestRestartCmdWindowsAlwaysTriesToStart(t *testing.T) {
	joined := strings.Join(restartCmd("windows"), " ")

	throw := strings.Index(joined, "throw 'ephemerd failed to start after retries'")
	if throw < 0 {
		t.Fatalf("windows restart no longer reports a failed start:\n%s", joined)
	}
	// Count start attempts that are sequenced BEFORE the throw. The loop is
	// one; the last-ditch attempt is the second.
	if n := strings.Count(joined[:throw], "Start-Service ephemerd"); n < 2 {
		t.Errorf("only %d Start-Service attempt(s) before the throw; a node whose stop outran the retry "+
			"loop gets abandoned in the Stopped state:\n%s", n, joined)
	}
}

// Linux is a different shape and must stay that way: systemctl restart blocks
// on the stop itself and applies systemd's own TimeoutStopSec, so it never had
// this bug. Pinned so the Windows fix is not "helpfully" generalised onto it.
func TestRestartCmdLinuxRestartsViaSystemd(t *testing.T) {
	joined := strings.Join(restartCmd("linux"), " ")
	if !strings.Contains(joined, "systemctl restart ephemerd") {
		t.Errorf("linux restart does not go through systemd:\n%s", joined)
	}
	if strings.Contains(joined, "WaitForStatus") {
		t.Errorf("linux restart picked up the Windows service dance:\n%s", joined)
	}
}
