package engine

import (
	"context"
	"fmt"
	"strings"

	"github.com/ephemerd/mayfly/internal/config"
	"github.com/ephemerd/mayfly/internal/provider"
	"github.com/ephemerd/mayfly/internal/state"
)

// upgradeExecTimeoutSec bounds how long apply watches the in-guest upgrade
// command before falling through to verification against the node. It is not
// a budget for the upgrade itself (nothing cancels the guest process).
const upgradeExecTimeoutSec = 600

// ProbeEphemerdVersion asks the node what it is running. Any failure answers
// "": an unreachable daemon, a build without --version, or a non-zero exit all
// mean the same thing to PlanNodeUpgrade: assume old, use the path that works
// everywhere.
func ProbeEphemerdVersion(ctx context.Context, ex NodeExecutor, osName string) string {
	res, err := ex.Exec(ctx, EphemerdVersionArgv(osName), 60)
	if err != nil || res.ExitCode != 0 {
		return ""
	}
	return strings.TrimSpace(res.Stdout + res.Stderr)
}

// instanceExecutor adapts an already-resolved provider.Instance to a
// NodeExecutor, so apply can drive the same upgrade + verification sequence
// `mayfly node upgrade` uses.
func instanceExecutor(inst provider.Instance, name, osName string) NodeExecutor {
	return &hypervisorExecutor{inst: inst, id: inst.ID(), name: name, os: osName}
}

// upgradeMember moves one member to the pool's selected ephemerd binary and
// records it in state once the NODE confirms it: the same probe → plan →
// exec → verify sequence as `mayfly node upgrade` (see nodeupgrade.go and
// nodeupgradewait.go for why the exec channel is not trusted as a witness).
//
// The daemon's self-upgrade drains in-process, so no idle wait precedes it.
// The binary-push path (a pinned binary_url, or a daemon too old to
// self-upgrade) restarts the service the blunt way, so it waits for the node
// to go idle first, like the config tier.
func (e *Engine) upgradeMember(ctx context.Context, ex NodeExecutor, pool *state.Pool, spec config.PoolSpec, member *state.Member) error {
	src := EphemerdSource(e.Cfg.Ephemerd, spec)
	reported := ProbeEphemerdVersion(ctx, ex, member.OS)
	up, err := PlanNodeUpgrade(src, member.OS, member.Arch, reported)
	if err != nil {
		return err
	}
	from := "unknown version"
	if reported != "" {
		from = firstLineOf(reported)
	}
	e.logf("pool %s: %s: upgrading ephemerd %s → %s (%s)",
		pool.Name, member.Hostname, from, describeBinary(BinaryRef(src)), up.Method)

	if up.Method == UpgradePush {
		e.waitForIdle(ctx, func(c context.Context) (string, error) {
			r, err := ex.Exec(c, statusCmd(member.OS), 60)
			if err != nil {
				return "", err
			}
			if r.ExitCode != 0 {
				return "", fmt.Errorf("exit %d: %s", r.ExitCode, firstLine(r.Stderr+r.Stdout))
			}
			return r.Stdout, nil
		}, pool.Name, member.Hostname, drainTimeout(spec))
	}

	// The exec's outcome is a hint, not the verdict.
	switch res, execErr := ex.Exec(ctx, up.Argv, upgradeExecTimeoutSec); {
	case execErr != nil:
		e.logf("pool %s: %s: lost sight of the upgrade command (%v); verifying against the node", pool.Name, member.Hostname, compactErr(execErr))
	case res.ExitCode != 0:
		e.logf("pool %s: %s: upgrade command exited %d; verifying against the node", pool.Name, member.Hostname, res.ExitCode)
	}

	target := up.Version
	if target == "" {
		target = "the pinned binary"
	}
	logf := func(format string, args ...any) {
		e.logf("pool %s: %s:"+format, append([]any{pool.Name, member.Hostname}, args...)...)
	}
	verdict, err := AwaitUpgrade(ctx, ex, member.OS, up.Version, UpgradeVerifyTimeout, logf)
	if err != nil {
		return err
	}
	if !verdict.Done() {
		return DescribeUpgradeFailure(verdict, target, UpgradeVerifyTimeout, EphemerdExe(member.OS))
	}
	var pem []byte
	if e.GH != nil {
		pem = e.GH.PrivateKeyPEM
	}
	RecordUpgrade(e.Cfg, e.St, pool, member, src, reported, pem)
	e.logf("pool %s: %s: upgraded: %s", pool.Name, member.Hostname, verdict.Health)
	return nil
}
