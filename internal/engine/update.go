package engine

import (
	"context"
	"fmt"
	"time"

	"github.com/luthermonson/mayfly/internal/config"
	"github.com/luthermonson/mayfly/internal/ephemerd"
	"github.com/luthermonson/mayfly/internal/provider"
	"github.com/luthermonson/mayfly/internal/state"
)

// updatePool applies in-place changes to a pool: an optional DNS hostname
// swap, then per-member work in tier order (config write → power cycle or
// restart → disk grow). State is saved after every member.
func (e *Engine) updatePool(ctx context.Context, u UpdateChange) error {
	pool, ok := e.St.FindPool(u.Name)
	if !ok {
		return fmt.Errorf("pool %s missing from state", u.Name)
	}

	if u.NewHostname != "" {
		e.logf("pool %s: repointing DNS %s → %s", u.Name, pool.Hostname, u.NewHostname)
		if err := e.CF.CreateDNSRoute(ctx, u.NewHostname, pool.TunnelID); err != nil {
			return err
		}
		oldHostname := pool.Hostname
		pool.Hostname = u.NewHostname
		if err := e.St.Save(); err != nil {
			return err
		}
		// Old CNAME removed only after the new one exists; tunnel is
		// unchanged so in-flight webhooks keep working throughout.
		if err := e.CF.DeleteTunnel(ctx, oldHostname, ""); err != nil {
			e.logf("pool %s: warning: could not remove old DNS %s: %v", u.Name, oldHostname, err)
		}
	}

	// Keep the pool's SSH transport settings converged with the spec so
	// key/port/password rotations in the TOML take effect on next apply.
	if u.Spec.UsesEnroll() {
		pool.SSHUser = u.Spec.SSHUser
		pool.SSHKey = u.Spec.SSHKey
		pool.SSHPort = u.Spec.SSHPort
		pool.SSHPassword = u.Spec.SSHPassword
		if err := e.St.Save(); err != nil {
			return err
		}
	}

	// Any member about to restart its daemon means running jobs are at risk:
	// cordon the pool's webhooks first so no new jobs land mid-update, wait
	// for each node to go idle, and reactivate the hooks when done — even on
	// error, or the pool's repos stay dark. Jobs that queue on GitHub while
	// cordoned are picked up by ephemerd's startup poll after its restart.
	//
	// An ephemerd upgrade on its own does NOT cordon: the daemon's
	// self-upgrade drains in-process, and deactivating the pool's webhooks
	// (which other pools share repos with) would stall the whole fleet's queue
	// for the length of every drain to protect jobs that are not at risk.
	restarts := false
	for _, mu := range u.Members {
		if mu.restartsDaemon() {
			restarts = true
			break
		}
	}
	timeout := drainTimeout(u.Spec)
	if restarts && timeout > 0 {
		refs := e.cordonPool(ctx, pool, u.Spec)
		defer e.uncordonPool(ctx, pool, refs)
	}

	for _, mu := range u.Members {
		if mu.Host != "" {
			if err := e.updateEnrolledMember(ctx, pool, u.Spec, mu, timeout); err != nil {
				return fmt.Errorf("member %s: %w", mu.Host, err)
			}
		} else if err := e.updateMember(ctx, pool, u.Spec, mu); err != nil {
			return fmt.Errorf("member %d: %w", mu.VMID, err)
		}
		if err := e.St.Save(); err != nil {
			return err
		}
	}
	return nil
}

// updateEnrolledMember converges one SSH-enrolled member. Config drift re-runs
// the idempotent enrollment (config, PEM, binary, restart), which also lands
// any new binary; a version bump alone is a self-upgrade over SSH.
func (e *Engine) updateEnrolledMember(ctx context.Context, pool *state.Pool, spec config.PoolSpec, mu MemberUpdate, timeout time.Duration) error {
	if mu.Config {
		e.waitForIdleSSH(ctx, pool, mu.Host, pool.Name, timeout)
		return e.enrollMember(ctx, pool, spec, mu.Host)
	}
	if !mu.Upgrade {
		return nil
	}
	member, ok := pool.FindMemberByHost(mu.Host)
	if !ok {
		return fmt.Errorf("%s missing from state", mu.Host)
	}
	ex := &sshExecutor{target: nodeSSHTarget(pool, spec, mu.Host), os: member.OS}
	return e.upgradeMember(ctx, ex, pool, spec, member)
}

func (e *Engine) updateMember(ctx context.Context, pool *state.Pool, spec config.PoolSpec, mu MemberUpdate) error {
	member, ok := pool.FindMember(mu.VMID)
	if !ok {
		return fmt.Errorf("vmid %d missing from state", mu.VMID)
	}
	prov, err := e.providerFor(spec)
	if err != nil {
		return err
	}
	inst, err := prov.GetInstance(ctx, member.PVENode, mu.VMID)
	if err != nil {
		return fmt.Errorf("look up vm: %w", err)
	}

	if mu.Config {
		e.logf("pool %s: vmid %d: writing config", pool.Name, mu.VMID)
		rendered, err := ephemerd.RenderConfig(e.configInputs(pool, spec, member.OS))
		if err != nil {
			return err
		}
		if err := inst.FileWrite(ctx, configPath(member.OS), []byte(rendered)); err != nil {
			return fmt.Errorf("write config.toml: %w", err)
		}
		if err := inst.FileWrite(ctx, pemPath(member.OS), e.GH.PrivateKeyPEM); err != nil {
			return fmt.Errorf("write app.pem: %w", err)
		}
	}

	switch {
	case mu.PowerCycle:
		e.logf("pool %s: vmid %d: power cycling (cores %d→%d, mem %d→%dMB)",
			pool.Name, mu.VMID, mu.FromCores, spec.Cores, mu.FromMemMB, spec.MemoryMB)
		e.waitForIdleInst(ctx, inst, member.OS, pool.Name, member.Hostname, drainTimeout(spec))
		if err := e.drainAndShutdown(ctx, inst, member.OS); err != nil {
			return err
		}
		if err := inst.SetResources(ctx, spec.Cores, spec.MemoryMB); err != nil {
			return err
		}
		if err := inst.Start(ctx); err != nil {
			return err
		}
		if err := inst.WaitForReady(ctx, 180); err != nil {
			return fmt.Errorf("guest agent after cycle: %w", err)
		}
		member.Cores = spec.Cores
		member.MemoryMB = spec.MemoryMB

	case mu.Config:
		// Config changed but no reboot needed: wait out running jobs (pool is
		// cordoned by updatePool), then restart ephemerd. The daemon-side
		// `drain` inside restartCmd is NOT a graceful wait — SIGTERM kills
		// running jobs via context cancellation — which is exactly why the
		// idle wait happens out here first.
		e.waitForIdleInst(ctx, inst, member.OS, pool.Name, member.Hostname, drainTimeout(spec))
		e.logf("pool %s: vmid %d: restarting ephemerd", pool.Name, mu.VMID)
		if err := e.guestExec(ctx, inst, restartCmd(member.OS), 600); err != nil {
			return fmt.Errorf("restart ephemerd: %w", err)
		}
	}

	if mu.Config {
		// Recorded as soon as the new files are live, so a failed upgrade
		// below does not also re-plan a config push that already happened.
		member.ConfigHash = ConfigHash(e.Cfg, e.St, spec, pool, e.GH.PrivateKeyPEM)
	}

	if mu.Upgrade {
		if err := e.upgradeMember(ctx, instanceExecutor(inst, member.Hostname, member.OS), pool, spec, member); err != nil {
			return fmt.Errorf("upgrade ephemerd: %w", err)
		}
	}

	if mu.DiskGrowGB > 0 {
		e.logf("pool %s: vmid %d: growing disk %dG→%dG (live)", pool.Name, mu.VMID, mu.FromDiskGB, mu.DiskGrowGB)
		if err := inst.ResizeDisk(ctx, mu.DiskGrowGB); err != nil {
			return err
		}
		if err := e.guestExec(ctx, inst, growCmd(member.OS), 120); err != nil {
			return fmt.Errorf("grow filesystem: %w", err)
		}
		member.DiskGB = mu.DiskGrowGB
	}
	return nil
}

func (e *Engine) drainAndShutdown(ctx context.Context, inst provider.Instance, osName string) error {
	// Best-effort drain; the provider's Shutdown owns the graceful-then-hard
	// fallback.
	_, _ = inst.Exec(ctx, drainCmd(osName), 330)
	return inst.Shutdown(ctx)
}

// configInputs assembles the ephemerd.ConfigInputs for one member of pool. It
// is the single source of truth for a node's rendered config, shared by
// provisionMember (cloud-init), enrollMember (SSH), updateMember and
// ReconfigureMember — so every path that puts a config.toml on a node renders
// it identically. osName selects the OS-specific bits (the app.pem path and the
// windows gateway-proxy gating): callers pass the recorded member.OS for an
// existing node, or spec.OS for a fresh clone; for a real member the two agree.
func (e *Engine) configInputs(pool *state.Pool, spec config.PoolSpec, osName string) ephemerd.ConfigInputs {
	return ephemerd.ConfigInputs{
		Owner:                 e.Cfg.GitHub.Owner,
		Repos:                 spec.Repos,
		AppID:                 e.GH.AppID,
		InstallationID:        e.GH.InstallationID,
		MaxConcurrent:         spec.MaxConcurrent,
		JobTimeout:            spec.JobTimeout,
		OrphanGrace:           spec.OrphanGrace,
		PrivateKeyPath:        pemPath(osName),
		TunnelToken:           pool.Token,
		TunnelHostname:        pool.Hostname,
		WebhookSecret:         pool.Secret,
		MetricsEnabled:        e.Cfg.MetricsEnabled(),
		MetricsPort:           e.Cfg.MetricsPort(),
		MetricsBindAddr:       e.Cfg.MetricsBindAddr(),
		DefaultImageLinux:     e.Cfg.GitHub.DefaultImageLinux,
		DefaultImageWindows:   e.Cfg.GitHub.DefaultImageWindows,
		DispatchPolicy:        dispatchPolicy(spec.ResolvedDispatchPolicy(e.Cfg.GitHub)),
		DindEnabled:           e.Cfg.Dind.Enabled,
		DindAllowPrivileged:   e.Cfg.Dind.AllowPrivileged,
		RegistryMirror:        registryMirror(e.Cfg, e.St),
		OS:                    osName,
		ModuleProxyEnabled:    e.Cfg.ModuleProxy.Enabled,
		CargoProxyEnabled:     e.Cfg.CargoProxy.Enabled,
		ComposerProxyEnabled:  e.Cfg.ComposerProxy.Enabled,
		NpmProxyEnabled:       e.Cfg.NpmProxy.Enabled,
		PipProxyEnabled:       e.Cfg.PipProxy.Enabled,
		PubProxyEnabled:       e.Cfg.PubProxy.Enabled,
		GhrelProxyEnabled:     e.Cfg.GhrelProxy.Enabled,
		ActionsCache:          actionsCache(e.Cfg, e.St, spec, osName),
		ExtraTargets:          extraTargets(spec, e.Cfg.GitHub),
		MacOSVM:               vmResources(spec.VM.MacOS),
		LinuxVM:               vmResources(spec.VM.Linux),
		WindowsRunnerMemoryMB: spec.WindowsRunner.MemoryMB,
		WindowsRunnerCPUs:     spec.WindowsRunner.CPUs,
	}
}

// ReconfigureMember re-renders one member's ephemerd config.toml + app.pem from
// its pool spec, pushes both to the node, and (unless restart is false) restarts
// ephemerd so the new config takes effect. It is `mayfly apply`'s config tier
// reduced to a single node: no plan, no drift refresh, and none of apply's VM
// lifecycle work (power-cycle, disk grow, scale) — so it keeps working when a
// full apply is blocked (a down node in the pool, a VLAN preflight failure) and
// touches nothing but the one node named. It is what `mayfly node reconfigure`
// runs, replacing the hand-sed-and-restart it used to take.
//
// Both transports are handled, mirroring the apply path: VM members over the
// hypervisor file/exec channel (updateMember), enrolled bare metal — darwin or
// linux — over SSH under sudo (enrollMember).
//
// On success the member's recorded ConfigHash is refreshed in memory so a later
// `mayfly plan` sees no spurious config drift; the caller persists state.
func (e *Engine) ReconfigureMember(ctx context.Context, poolName string, member *state.Member, restart bool) error {
	pool, ok := e.St.FindPool(poolName)
	if !ok {
		return fmt.Errorf("pool %s missing from state", poolName)
	}
	spec, ok := resolvedPoolSpec(e.Cfg, poolName)
	if !ok {
		return fmt.Errorf("pool %s is not in config, so its config cannot be re-rendered", poolName)
	}

	if member.VMID == 0 {
		if err := e.reconfigureEnrolled(ctx, pool, spec, member, restart); err != nil {
			return err
		}
	} else {
		prov, err := e.providerFor(spec)
		if err != nil {
			return err
		}
		inst, err := prov.GetInstance(ctx, member.PVENode, member.VMID)
		if err != nil {
			return fmt.Errorf("look up vm %d: %w", member.VMID, err)
		}
		if err := e.reconfigureInstance(ctx, pool, spec, member.OS, inst, restart); err != nil {
			return err
		}
	}

	member.ConfigHash = ConfigHash(e.Cfg, e.St, spec, pool, e.GH.PrivateKeyPEM)
	return nil
}

// reconfigureInstance renders the pool config for osName and pushes config.toml
// + app.pem to a VM member over its hypervisor file channel, restarting ephemerd
// afterward unless restart is false. Split out from ReconfigureMember so it runs
// against any provider.Instance — the real one from GetInstance in production, a
// capturing fake in tests.
func (e *Engine) reconfigureInstance(ctx context.Context, pool *state.Pool, spec config.PoolSpec, osName string, inst provider.Instance, restart bool) error {
	rendered, err := ephemerd.RenderConfig(e.configInputs(pool, spec, osName))
	if err != nil {
		return err
	}
	e.logf("pool %s: vmid %d: writing config", pool.Name, inst.ID())
	if err := inst.FileWrite(ctx, configPath(osName), []byte(rendered)); err != nil {
		return fmt.Errorf("write config.toml: %w", err)
	}
	if err := inst.FileWrite(ctx, pemPath(osName), e.GH.PrivateKeyPEM); err != nil {
		return fmt.Errorf("write app.pem: %w", err)
	}
	if !restart {
		return nil
	}
	// Unlike apply's config tier, no idle-wait/cordon precedes this: the operator
	// asked to reconfigure this one node now. restartCmd's own `ephemerd drain`
	// still stops the daemon gracefully before the service bounce.
	e.logf("pool %s: vmid %d: restarting ephemerd", pool.Name, inst.ID())
	if err := e.guestExec(ctx, inst, restartCmd(osName), 600); err != nil {
		return fmt.Errorf("restart ephemerd: %w", err)
	}
	return nil
}

// reconfigureEnrolled is the SSH counterpart of reconfigureInstance for a
// bare-metal member: it writes config.toml + app.pem and (re)starts the service
// exactly the way enrollMember does, but without the binary download/install —
// the box is already enrolled, only its config changed.
func (e *Engine) reconfigureEnrolled(ctx context.Context, pool *state.Pool, spec config.PoolSpec, member *state.Member, restart bool) error {
	rendered, err := ephemerd.RenderConfig(e.configInputs(pool, spec, member.OS))
	if err != nil {
		return err
	}
	target := nodeSSHTarget(pool, spec, member.Hostname)
	e.logf("pool %s: %s: writing config via ssh", pool.Name, member.Hostname)
	if _, err := SSHRun(ctx, target, "cat > /var/lib/ephemerd/config.toml && chmod 600 /var/lib/ephemerd/config.toml", []byte(rendered)); err != nil {
		return err
	}
	if _, err := SSHRun(ctx, target, "cat > /etc/ephemerd/app.pem && chmod 600 /etc/ephemerd/app.pem", e.GH.PrivateKeyPEM); err != nil {
		return err
	}
	if !restart {
		return nil
	}
	e.logf("pool %s: %s: restarting ephemerd", pool.Name, member.Hostname)
	_, err = SSHRun(ctx, target, enrollServiceStart(member.OS), nil)
	return err
}

func (e *Engine) guestExec(ctx context.Context, inst provider.Instance, argv []string, timeoutSec int) error {
	status, err := inst.Exec(ctx, argv, timeoutSec)
	if err != nil {
		return err
	}
	if status.ExitCode != 0 {
		return fmt.Errorf("exit code %d: %s", status.ExitCode, status.ErrData)
	}
	return nil
}
