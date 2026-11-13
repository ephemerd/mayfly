package engine

// Verifying that an upgrade actually happened.
//
// `mayfly node upgrade` used to report whatever the in-guest exec channel
// reported. That channel is not a witness to the upgrade — it is a witness to
// itself, and the two disagree in both directions:
//
//   - It reports failure on success. An ephemerd upgrade restarts the node's
//     services, and a restarting guest stops answering qemu-guest-agent for a
//     few seconds. The poll errors out; the upgrade completes anyway, because
//     nothing on the guest side is listening to whether mayfly is still
//     watching. On 2026-08-16 both Windows upgrades (v0.2.2 and v0.2.3) landed
//     cleanly while mayfly printed "guest-exec-status failed - got timeout"
//     for each. The operator concluded the second had failed. It had not.
//   - It could report success on failure. An `ephemerd upgrade` that exits 0
//     has only handed the restart to the service manager; the daemon may still
//     never come back on the new binary (a known Windows failure mode), and a
//     zero exit says nothing about that.
//
// So the exec's outcome is downgraded to a progress signal and the verdict
// comes from the node itself: poll `ephemerd status` until it reports the
// target version AND is serving jobs again. Those are the two facts an
// operator would otherwise have to go check by hand, and they are the two
// facts that define a finished upgrade.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// upgradeVerifyPoll is the gap between status probes. Each one is a full exec
// round trip through the guest agent, and the daemon takes seconds to come
// back, so there is nothing to gain from going faster. A var so tests can
// shrink it.
var upgradeVerifyPoll = 5 * time.Second

const (
	// UpgradeVerifyTimeout is how long to wait for a node to come back on
	// the new version.
	//
	// Sized against the daemon's own restart supervision rather than a round
	// number: ephemerd asks the service manager twice with a 90s watchdog on
	// each, so it takes ~3 minutes to decide a restart is never happening —
	// and only then does it un-cordon itself. Giving up before that would
	// report a drained node that is about to un-drain.
	UpgradeVerifyTimeout = 6 * time.Minute

	// statusExecTimeout bounds one `ephemerd status` probe.
	statusExecTimeout = 60
)

// NodeHealth is the subset of `ephemerd status` that says whether an upgrade
// finished. Uptime is carried because it is the tiebreaker a human reaches
// for: a daemon reporting the new version with hours of uptime never
// restarted into it.
type NodeHealth struct {
	Status     string `json:"status"`
	Version    string `json:"version"`
	Draining   bool   `json:"draining"`
	ActiveJobs int    `json:"active_jobs"`
	Uptime     string `json:"uptime"`
}

// String renders the health for an operator-facing message.
func (h NodeHealth) String() string {
	serving := "serving jobs"
	if h.Draining {
		serving = "DRAINED (not accepting jobs)"
	}
	return fmt.Sprintf("version %s, %s, %d active job(s), up %s",
		orUnknown(h.Version), serving, h.ActiveJobs, orUnknown(h.Uptime))
}

func orUnknown(s string) string {
	if strings.TrimSpace(s) == "" {
		return "unknown"
	}
	return s
}

// ProbeNodeHealth runs `ephemerd status` on a node and parses it.
func ProbeNodeHealth(ctx context.Context, ex NodeExecutor, osName string) (NodeHealth, error) {
	res, err := ex.Exec(ctx, EphemerdArgv(osName, "status"), statusExecTimeout)
	if err != nil {
		return NodeHealth{}, err
	}
	if res.ExitCode != 0 {
		return NodeHealth{}, fmt.Errorf("ephemerd status exited %d: %s",
			res.ExitCode, strings.TrimSpace(firstNonEmptyLine(res.Stderr, res.Stdout)))
	}
	return parseNodeHealth(res.Stdout)
}

// parseNodeHealth pulls the status JSON out of a command's stdout.
//
// It slices between the first '{' and last '}' rather than unmarshalling the
// whole stream. On the path this package uses that is pure defensiveness —
// EphemerdArgv execs the daemon binary directly through the guest agent, with
// no shell in the chain, so stdout is the daemon's pretty-printed JSON and
// nothing else. It is cheap insurance for the channels where that is not true:
// SSH merges stderr into stdout, and anything routed through
// `powershell -Command` picks up a CLIXML progress-stream preamble. In those
// cases the JSON is intact, it just is not alone.
func parseNodeHealth(stdout string) (NodeHealth, error) {
	start := strings.Index(stdout, "{")
	end := strings.LastIndex(stdout, "}")
	if start < 0 || end < start {
		return NodeHealth{}, fmt.Errorf("no status JSON in output: %q", truncate(stdout, 200))
	}
	var h NodeHealth
	if err := json.Unmarshal([]byte(stdout[start:end+1]), &h); err != nil {
		return NodeHealth{}, fmt.Errorf("parsing ephemerd status: %w", err)
	}
	return h, nil
}

// UpgradeVerdict is the verified outcome of a node upgrade.
//
// Fresh is the field that keeps the rest honest. Health, Reached and Serving
// describe the last status that was successfully read, which is not the same
// as the node's state now: a node that answers once and then goes silent would
// otherwise be described forever by that one stale sample. Only Fresh says the
// description is current.
type UpgradeVerdict struct {
	// Health is the last status successfully read from the node. Zero when
	// the node never answered.
	Health NodeHealth
	// Reached is true when Health reports the version that was asked for
	// (always true when no specific version was pinned).
	Reached bool
	// Serving is true when Health says the node is not draining.
	Serving bool
	// Answered is true when the node's daemon replied at least once.
	Answered bool
	// Fresh is true when the FINAL probe succeeded, i.e. Health describes the
	// node as of the last thing we asked it. False means Health is a stale
	// snapshot and nothing in this verdict may be stated as present tense.
	Fresh bool
	// LastErr is why the final probe failed. Nil when Fresh.
	LastErr error
}

// Done reports whether the upgrade finished: right version, taking jobs, and
// verified by the last probe rather than an older one.
func (v UpgradeVerdict) Done() bool { return v.Fresh && v.Reached && v.Serving }

// InFlight reports that the node is visibly still working on the upgrade: it
// is answering, has not reached the target version, and is cordoned. That is
// what ephemerd looks like while it drains running jobs (up to 45 minutes by
// default) or downloads the release — a healthy upgrade in progress, not a
// failed one.
func (v UpgradeVerdict) InFlight() bool { return v.Fresh && !v.Reached && v.Health.Draining }

// AwaitUpgrade polls a node until it reports wantVersion and is serving jobs
// again, or until timeout. An empty wantVersion (a binary_url-pinned install,
// which names an artifact rather than a tag) asserts only that the daemon came
// back and is serving.
//
// It never returns an error for "not there yet" — the verdict carries the
// facts and the caller decides what to say about them. The error return is for
// a caller-cancelled context only.
func AwaitUpgrade(ctx context.Context, ex NodeExecutor, osName, wantVersion string, timeout time.Duration, logf func(string, ...any)) (UpgradeVerdict, error) {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	deadline := time.Now().Add(timeout)
	var v UpgradeVerdict
	reported := ""

	for {
		h, err := ProbeNodeHealth(ctx, ex, osName)
		if err == nil {
			v.Answered = true
			v.Fresh = true
			v.LastErr = nil
			v.Health = h
			v.Reached = wantVersion == "" || sameVersion(h.Version, wantVersion)
			v.Serving = !h.Draining
			if v.Done() {
				return v, nil
			}
			if h.Version != reported {
				reported = h.Version
				logf("  node reports %s", h)
			}
		} else {
			// Do NOT keep describing the node from an older sample. Whatever
			// Health still holds is now history, and the whole point of this
			// function is to stop mayfly making confident claims about a node
			// it cannot see.
			v.Fresh = false
			v.LastErr = err
			// Expected while the service is stopped mid-restart.
			logf("  node not answering yet (%v)", compactErr(err))
		}

		if time.Now().After(deadline) {
			return v, nil
		}
		select {
		case <-ctx.Done():
			return v, ctx.Err()
		case <-time.After(upgradeVerifyPoll):
		}
	}
}

// DescribeUpgradeFailure turns a verdict that did not reach Done into the
// message an operator needs: what the node actually is, and what to do next.
//
// The buckets exist because they have genuinely different remedies, and
// because the previous single "it failed" reading is what this whole change
// set is trying to abolish. Two of them are NOT failures of the upgrade:
// UNVERIFIED means we lost sight of it, IN PROGRESS means it is still running.
// Neither should send anyone to read a log that says nothing is wrong.
func DescribeUpgradeFailure(v UpgradeVerdict, wantVersion string, timeout time.Duration, exe string) error {
	switch {
	case !v.Fresh:
		// Covers both "never answered" and "answered, then went silent". The
		// second is the more dangerous one to get wrong: it is what a swap
		// that never restarts looks like, and the last sample we hold is a
		// mid-upgrade snapshot that would read as a confident, false verdict.
		msg := fmt.Sprintf("upgrade UNVERIFIED: the node's daemon was not answering after %s\n"+
			"  the upgrade command was delivered; whether it completed is unknown", timeout)
		if v.Answered {
			msg += fmt.Sprintf("\n  last seen (STALE, before it went quiet): %s", v.Health)
		} else {
			msg += "\n  it never answered at all during the wait"
		}
		if v.LastErr != nil {
			msg += fmt.Sprintf("\n  last probe error: %s", compactErr(v.LastErr))
		}
		msg += "\n  check:  mayfly node status <node>   (and the node's ephemerd log)"
		return errors.New(msg)

	case v.InFlight():
		// ephemerd drains running jobs BEFORE it downloads, waiting up to 45
		// minutes by default, and answers status with the old version and
		// draining=true throughout. Calling that a failure would be the same
		// false negative this command was changed to stop producing.
		waiting := "installing the new binary"
		if v.Health.ActiveJobs > 0 {
			waiting = fmt.Sprintf("waiting for %d running job(s) to finish", v.Health.ActiveJobs)
		}
		return fmt.Errorf("upgrade STILL IN PROGRESS after %s — not a failure\n"+
			"  the node is cordoned and %s; ephemerd allows up to 45m for the drain\n"+
			"  node: %s\n"+
			"  wait longer:  mayfly node upgrade <node> --wait 50m\n"+
			"  or check later:  mayfly node status <node>",
			timeout, waiting, v.Health)

	case !v.Reached:
		return fmt.Errorf("upgrade DID NOT HAPPEN: node still reports %s after %s (wanted %s)\n"+
			"  it is serving jobs on the old binary, so nothing is stuck\n"+
			"  node: %s\n"+
			"  check the node's ephemerd log for why the upgrade or its restart failed",
			orUnknown(v.Health.Version), timeout, wantVersion, v.Health)

	case !v.Serving:
		// Rare by construction: a node reporting the NEW version has restarted,
		// and `draining` lives only in the scheduler's memory, so a fresh
		// daemon starts un-cordoned. Reaching here means something cordoned it
		// AFTER the upgrade landed — a concurrent `ephemerd drain`, or an
		// upgrade that was a no-op on an already-cordoned node. Cheap
		// insurance, not the expected outcome.
		return fmt.Errorf("upgrade INCOMPLETE: node is running %s but is DRAINED after %s\n"+
			"  the upgrade landed; something is holding the cordon, so it takes no jobs\n"+
			"  node: %s\n"+
			"  fix:  mayfly node exec <node> -- %s uncordon",
			v.Health.Version, timeout, v.Health, exe)

	default:
		// Unreachable via Done()-guarded callers, but this is exported.
		return fmt.Errorf("upgrade could not be verified after %s\n  node: %s", timeout, v.Health)
	}
}

// sameVersion compares two version strings by their numeric triple, so
// "ephemerd version v0.2.3" and "v0.2.3" match.
func sameVersion(a, b string) bool {
	va, oka := parseVersion(a)
	vb, okb := parseVersion(b)
	return oka && okb && va == vb
}

// EphemerdExe is the installed daemon path for an OS, for remediation hints.
func EphemerdExe(osName string) string {
	if osName == "windows" {
		return winExe
	}
	return unixExe
}

func firstNonEmptyLine(candidates ...string) string {
	for _, c := range candidates {
		if s := strings.TrimSpace(c); s != "" {
			return firstLineOf(s)
		}
	}
	return ""
}

func firstLineOf(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// compactErr shortens a multi-line transport error for a progress line.
func compactErr(err error) string { return truncate(firstLineOf(err.Error()), 120) }
