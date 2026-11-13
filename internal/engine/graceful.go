package engine

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/ephemerd/mayfly/internal/config"
	"github.com/ephemerd/mayfly/internal/github"
	"github.com/ephemerd/mayfly/internal/provider"
	"github.com/ephemerd/mayfly/internal/state"
)

// Graceful restart: cordon → wait idle → restart → uncordon.
//
// ephemerd has no cordon of its own — its only "stop taking jobs" lever is
// SIGTERM, and SIGTERM context-cancels every running job before drain() gets
// to wait (the v0.6.1 release lost 4 CI legs to exactly that). Until the
// daemon grows a real Cordon RPC, mayfly cordons at the source instead: it
// deactivates the pool's GitHub webhooks, so no queued events reach the
// daemon at all while apply waits for running jobs to finish. Events that
// fire while the hooks are dark are not lost — ephemerd's startup poll
// ("checking for queued jobs") picks them up after the restart.
//
// Known leak: a daemon configured with a reconcile interval PULLS queued jobs
// from GitHub periodically, hook or no hook. waitForIdle re-checks the count
// immediately before returning to shrink that window back to seconds.

// defaultDrainTimeout bounds the idle wait when the pool doesn't set
// drain_timeout. Sized above the fleet's longest routine job (Windows release
// legs run ~30m) so the timeout firing means something is actually wrong.
const defaultDrainTimeout = 45 * time.Minute

// drainTimeout resolves a pool's drain_timeout (validated at config load).
func drainTimeout(spec config.PoolSpec) time.Duration {
	if spec.DrainTimeout == "" {
		return defaultDrainTimeout
	}
	d, err := time.ParseDuration(spec.DrainTimeout)
	if err != nil {
		return defaultDrainTimeout
	}
	return d
}

// hookRef records one webhook the cordon deactivated, with a client holding
// the credential that can flip it back.
type hookRef struct {
	gh    *github.Client
	owner string
	repo  string
	id    int64
}

// cordonPool deactivates every active webhook that points at the pool's
// hostname, across the primary [github] owner's repos and all github_extra
// targets. Best-effort by design: a repo that fails to cordon only re-opens
// the small dispatch race for that repo, which is still strictly better than
// aborting the apply. Returns the refs uncordonPool needs.
func (e *Engine) cordonPool(ctx context.Context, pool *state.Pool, spec config.PoolSpec) []hookRef {
	var refs []hookRef
	for _, t := range e.hookTargets(ctx, spec) {
		for _, repo := range t.repos {
			hooks, err := t.gh.ListRepoHooks(ctx, t.owner, repo)
			if err != nil {
				e.logf("pool %s: cordon: list hooks %s/%s: %v (skipping)", pool.Name, t.owner, repo, err)
				continue
			}
			for _, h := range hooks {
				if !h.Active || !hookPointsAt(h.Config.URL, pool.Hostname) {
					continue
				}
				if err := t.gh.SetRepoHookActive(ctx, t.owner, repo, h.ID, false); err != nil {
					e.logf("pool %s: cordon: deactivate hook %s/%s #%d: %v (skipping)", pool.Name, t.owner, repo, h.ID, err)
					continue
				}
				refs = append(refs, hookRef{gh: t.gh, owner: t.owner, repo: repo, id: h.ID})
			}
		}
	}
	if len(refs) > 0 {
		e.logf("pool %s: cordoned %d webhook(s); new jobs will queue on GitHub until restart completes", pool.Name, len(refs))
	}
	return refs
}

// uncordonPool reactivates cordoned hooks. Failures are loud: a hook left
// inactive means that repo's CI is dead until someone notices, so every
// failure names the exact hook to fix by hand. (ephemerd also re-adopts hooks
// at startup, which usually rights this on its own — logged as belt, not
// relied on as suspenders.)
func (e *Engine) uncordonPool(ctx context.Context, pool *state.Pool, refs []hookRef) {
	for _, r := range refs {
		if err := r.gh.SetRepoHookActive(ctx, r.owner, r.repo, r.id, true); err != nil {
			e.logf("pool %s: UNCORDON FAILED for %s/%s hook #%d: %v — reactivate it by hand (repo settings → webhooks) or repo CI stays dark",
				pool.Name, r.owner, r.repo, r.id, err)
		}
	}
	if len(refs) > 0 {
		e.logf("pool %s: uncordoned %d webhook(s)", pool.Name, len(refs))
	}
}

// hookTarget pairs an authenticated client with the repos it can see.
type hookTarget struct {
	gh    *github.Client
	owner string
	repos []string
}

// hookTargets assembles the (client, owner, repos) tuples a cordon must
// visit: the primary App-authenticated owner plus PAT-authenticated extras.
func (e *Engine) hookTargets(ctx context.Context, spec config.PoolSpec) []hookTarget {
	var targets []hookTarget
	if len(spec.Repos) > 0 && e.GH != nil {
		token, err := e.GH.InstallationToken(ctx)
		if err != nil {
			e.logf("cordon: mint installation token: %v (skipping %s repos)", err, e.Cfg.GitHub.Owner)
		} else {
			targets = append(targets, hookTarget{gh: github.New(token), owner: e.Cfg.GitHub.Owner, repos: spec.Repos})
		}
	}
	for _, extra := range spec.GitHubExtra {
		token := extra.ResolvedToken()
		if token == "" {
			e.logf("cordon: github_extra %s: no token resolved (skipping)", extra.Owner)
			continue
		}
		targets = append(targets, hookTarget{gh: github.New(token), owner: extra.Owner, repos: extra.Repos})
	}
	return targets
}

// hookPointsAt reports whether a webhook URL targets the pool's hostname
// (e.g. https://ci-linux-amd64.tricorder.cc/webhook/github/ephpm).
func hookPointsAt(url, hostname string) bool {
	if hostname == "" {
		return false
	}
	return strings.Contains(url, "://"+hostname+"/") || strings.HasSuffix(url, "://"+hostname)
}

// activeJobsRe extracts active_jobs from `ephemerd status` JSON output.
var activeJobsRe = regexp.MustCompile(`"active_jobs":\s*(\d+)`)

// parseActiveJobs pulls the running-job count out of `ephemerd status`.
func parseActiveJobs(out string) (int, error) {
	m := activeJobsRe.FindStringSubmatch(out)
	if m == nil {
		return 0, fmt.Errorf("no active_jobs in status output: %q", firstLine(out))
	}
	return strconv.Atoi(m[1])
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// statusCmd asks the local daemon for its status JSON.
func statusCmd(osName string) []string {
	return EphemerdArgv(osName, "status")
}

// waitForIdle polls the node's active-job count until it reaches zero or the
// timeout expires. Assumes the pool is already cordoned — without that, a job
// can land between the last poll and the restart. A node whose status probe
// errors (daemon down, pre-status version) is treated as idle after one
// retry: blocking an apply on a broken daemon would deadlock the exact
// recovery path that fixes it.
func (e *Engine) waitForIdle(ctx context.Context, run func(context.Context) (string, error), poolName, node string, timeout time.Duration) {
	if timeout <= 0 {
		return
	}
	deadline := time.Now().Add(timeout)
	failures := 0
	lastLog := time.Time{}
	for {
		out, err := run(ctx)
		if err != nil {
			failures++
			if failures >= 2 {
				e.logf("pool %s: %s: status probe failing (%v); treating as idle", poolName, node, err)
				return
			}
		} else {
			failures = 0
			n, perr := parseActiveJobs(out)
			if perr != nil {
				e.logf("pool %s: %s: %v; treating as idle", poolName, node, perr)
				return
			}
			if n == 0 {
				return
			}
			if time.Since(lastLog) >= time.Minute {
				e.logf("pool %s: %s: waiting for %d running job(s) to finish (up to %s more)",
					poolName, node, n, time.Until(deadline).Round(time.Second))
				lastLog = time.Now()
			}
		}
		if time.Now().After(deadline) {
			e.logf("pool %s: %s: DRAIN TIMEOUT after %s — restarting with jobs still running (they will be killed and must be re-run)",
				poolName, node, timeout)
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(15 * time.Second):
		}
	}
}

// waitForIdleInst adapts waitForIdle to a guest-agent instance.
func (e *Engine) waitForIdleInst(ctx context.Context, inst provider.Instance, osName, poolName, node string, timeout time.Duration) {
	e.waitForIdle(ctx, func(c context.Context) (string, error) {
		st, err := inst.Exec(c, statusCmd(osName), 60)
		if err != nil {
			return "", err
		}
		if st.ExitCode != 0 {
			return "", fmt.Errorf("exit %d: %s", st.ExitCode, firstLine(st.ErrData))
		}
		return st.OutData, nil
	}, poolName, node, timeout)
}

// waitForIdleSSH adapts waitForIdle to an SSH-enrolled (darwin/bare-metal)
// member.
func (e *Engine) waitForIdleSSH(ctx context.Context, pool *state.Pool, host, poolName string, timeout time.Duration) {
	e.waitForIdle(ctx, func(c context.Context) (string, error) {
		return SSHRun(c, sshTarget(pool, host), "/usr/local/bin/ephemerd status", nil)
	}, poolName, host, timeout)
}
