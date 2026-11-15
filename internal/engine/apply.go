package engine

import (
	"context"
	"fmt"
	"time"

	"github.com/luthermonson/mayfly/internal/cloudinit"
	"github.com/luthermonson/mayfly/internal/config"
	"github.com/luthermonson/mayfly/internal/ephemerd"
	"github.com/luthermonson/mayfly/internal/provider"
	"github.com/luthermonson/mayfly/internal/pve"
	"github.com/luthermonson/mayfly/internal/state"
)

func (e *Engine) createPool(ctx context.Context, c PoolChange) error {
	e.logf("pool %s: creating tunnel", c.Name)
	tunnel, err := e.CF.CreateTunnel(ctx, c.Name)
	if err != nil {
		return err
	}
	if err := e.CF.CreateDNSRoute(ctx, c.Spec.Hostname, tunnel.ID); err != nil {
		_ = e.CF.DeleteTunnel(ctx, c.Spec.Hostname, tunnel.ID)
		return err
	}

	pool := state.Pool{
		Name:        c.Name,
		Hostname:    c.Spec.Hostname,
		TunnelID:    tunnel.ID,
		Token:       tunnel.Token,
		Secret:      randomHex(32),
		SSHUser:     c.Spec.SSHUser,
		SSHKey:      c.Spec.SSHKey,
		SSHPort:     c.Spec.SSHPort,
		SSHPassword: c.Spec.SSHPassword,
	}
	e.St.Pools = append(e.St.Pools, pool)
	if err := e.St.Save(); err != nil {
		return err
	}

	return e.scaleUp(ctx, ScaleChange{Name: c.Name, Spec: c.Spec, Count: c.Spec.Size, Hosts: c.Spec.Members})
}

func (e *Engine) scaleUp(ctx context.Context, s ScaleChange) error {
	pool, ok := e.St.FindPool(s.Name)
	if !ok {
		return fmt.Errorf("pool %s missing from state", s.Name)
	}
	if s.Spec.UsesEnroll() {
		for _, host := range s.Hosts {
			if err := e.enrollMember(ctx, pool, s.Spec, host); err != nil {
				return err
			}
			if err := e.St.Save(); err != nil {
				return err
			}
		}
		return nil
	}
	for i := 0; i < s.Count; i++ {
		if err := e.provisionMember(ctx, pool, s.Spec); err != nil {
			return err
		}
		if err := e.St.Save(); err != nil {
			return err
		}
	}
	return nil
}

func (e *Engine) scaleDown(ctx context.Context, s ScaleChange) error {
	pool, ok := e.St.FindPool(s.Name)
	if !ok {
		return fmt.Errorf("pool %s missing from state", s.Name)
	}
	for _, host := range s.Hosts {
		if err := e.unenrollMember(ctx, pool, host); err != nil {
			return err
		}
		if err := e.St.Save(); err != nil {
			return err
		}
	}
	for _, vmid := range s.VMIDs {
		if err := e.destroyMember(ctx, pool, vmid); err != nil {
			return err
		}
		if err := e.St.Save(); err != nil {
			return err
		}
	}
	return nil
}

// replaceMembers does a rolling replace: for each stale member, provision a
// fresh one first, then drain and destroy the old one. Capacity never dips
// below the pool's size during the roll.
func (e *Engine) replaceMembers(ctx context.Context, r ScaleChange) error {
	pool, ok := e.St.FindPool(r.Name)
	if !ok {
		return fmt.Errorf("pool %s missing from state", r.Name)
	}
	for _, vmid := range r.VMIDs {
		e.logf("pool %s: replacing member %d", r.Name, vmid)
		if err := e.provisionMember(ctx, pool, r.Spec); err != nil {
			return fmt.Errorf("provision replacement: %w", err)
		}
		if err := e.St.Save(); err != nil {
			return err
		}
		if err := e.destroyMember(ctx, pool, vmid); err != nil {
			return fmt.Errorf("retire member %d: %w", vmid, err)
		}
		if err := e.St.Save(); err != nil {
			return err
		}
	}
	return nil
}

func (e *Engine) provisionMember(ctx context.Context, pool *state.Pool, spec config.PoolSpec) error {
	prov, err := e.providerFor(spec)
	if err != nil {
		return err
	}
	vmid, err := prov.NextID(ctx)
	if err != nil {
		return fmt.Errorf("next vmid: %w", err)
	}
	name := fmt.Sprintf("mfl-%s-%d", pool.Name, vmid)
	pveNode, pveStorage := spec.ResolvedPlacement(e.Cfg.Proxmox)
	e.logf("pool %s: provisioning %s (vmid %d, %s/%s on %s)", pool.Name, name, vmid, spec.OS, spec.Arch, pveNode)

	binURL, err := resolveEphemerdURL(EphemerdSource(e.Cfg.Ephemerd, spec), spec.OS, spec.Arch)
	if err != nil {
		return err
	}

	ephCfg, err := ephemerd.RenderConfig(e.configInputs(pool, spec, spec.OS))
	if err != nil {
		return err
	}

	rendered, err := cloudinit.Render(cloudinit.Inputs{
		Hostname:          name,
		OS:                spec.OS,
		GitHubAppPEM:      e.GH.PrivateKeyPEM,
		EphemerdConfig:    ephCfg,
		EphemerdBinaryURL: binURL,
	})
	if err != nil {
		return err
	}

	inst, err := prov.CreateInstance(ctx, provider.CreateSpec{
		ID:       vmid,
		Name:     name,
		Template: spec.Template,
		Node:     pveNode,
		Storage:  pveStorage,
		// vm_storage set → FULL clone with the disk on that storage; empty →
		// LINKED clone on the template's storage (the historical behavior).
		VMStorage: spec.VMStorage,
		Arch:      spec.Arch,
		Cores:     spec.Cores,
		MemoryMB:  spec.MemoryMB,
		DiskGB:    spec.DiskGB,
		Tags:      []string{pve.TagMayfly, pve.TagEphemerd, "mayfly-pool-" + pool.Name},
		UserData:  rendered.UserData,
		MetaData:  rendered.MetaData,
		// Per-pool network isolation: put the runner vNIC on a tagged VLAN
		// and/or turn on the hypervisor firewall for it. Both zero = the
		// historical flat-LAN, no-firewall NIC.
		VLANTag:  spec.VLANTag,
		Firewall: spec.Firewall,
		// The cloud-init ISO can live on a different storage than the VM disk
		// (block stores reject 'iso' content). Empty = keep it on the disk
		// storage, the historical behavior.
		CloudInitStorage: spec.ResolvedCloudInitStorage(e.Cfg.Proxmox),
	})
	if err != nil {
		return err
	}

	e.logf("pool %s: starting %s", pool.Name, name)
	if err := inst.Start(ctx); err != nil {
		return fmt.Errorf("start %s: %w", name, err)
	}

	e.logf("pool %s: waiting for guest agent on %s", pool.Name, name)
	if err := inst.WaitForReady(ctx, 180); err != nil {
		return fmt.Errorf("guest agent on %s: %w", name, err)
	}

	// Smoke-test guest-exec specifically: WaitForReady proves the exec channel
	// answers, not that exec RPCs are allowed. Everything mayfly does later
	// (status, drain, upgrade) rides guest-exec.
	e.logf("pool %s: guest agent up on %s; checking guest exec", pool.Name, name)
	if _, err := inst.Exec(ctx, smokeCmd(spec.OS), 30); err != nil {
		return fmt.Errorf("guest-exec smoke test on %s: %w", name, err)
	}

	// Record the LAN IP so the monitoring node can scrape :9090. Best-effort:
	// never fail provisioning over it — the address is refreshed on later
	// applies via Refresh. When something scrapes it, give DHCP a short window
	// so this same apply's monitoring step already includes the new member.
	memberIP := e.memberIPv4(ctx, inst)
	ipNote := memberIP
	if ipNote == "" {
		ipNote = "not reported yet; picked up by a later refresh"
	}

	pool.Members = append(pool.Members, state.Member{
		VMID:     vmid,
		Hostname: name,
		PVENode:  pveNode,
		IP:       memberIP,
		OS:       spec.OS,
		Arch:     spec.Arch,
		Cores:    spec.Cores,
		MemoryMB: spec.MemoryMB,
		DiskGB:   spec.DiskGB,
		Template: spec.Template,
		// NIC isolation written on the clone (see CreateSpec above). Recording
		// it is what lets the planner notice a later vlan_tag/firewall change
		// and force a replace — without it a changed tag produces no plan.
		VLANTag:  spec.VLANTag,
		Firewall: spec.Firewall,
		// Disk-placement storage written on the clone (empty = linked clone).
		// Recorded for the same reason as VLANTag: a later vm_storage change
		// must force a replace to migrate the existing member's disk.
		VMStorage:  spec.VMStorage,
		ConfigHash: ConfigHash(e.Cfg, e.St, spec, pool, e.GH.PrivateKeyPEM),
		Ephemerd:   DesiredBinary(e.Cfg, spec),
		CreatedAt:  time.Now().UTC(),
	})
	// cloud-init installs and starts ephemerd on first boot, asynchronously;
	// mayfly's part ends here. Say so, so a quiet apply is not read as a hang.
	e.logf("pool %s: provisioned %s (vmid %d, ip %s); ephemerd is installed and started by cloud-init on first boot",
		pool.Name, name, vmid, ipNote)
	return nil
}

// memberIPWait bounds how long provisioning waits for a fresh guest to report
// a LAN address when monitoring will scrape it. A var so tests can shrink it.
var (
	memberIPWait = 30 * time.Second
	memberIPPoll = 3 * time.Second
)

// memberIPv4 reads a new member's LAN IPv4. Without monitoring nothing scrapes
// the address, so one read is enough; with it, a short bounded wait lets the
// same apply publish the member as a scrape target instead of leaving it for
// the next one.
func (e *Engine) memberIPv4(ctx context.Context, inst provider.Instance) string {
	deadline := time.Now().Add(memberIPWait)
	for {
		if ip, err := inst.PrimaryIPv4(ctx); err == nil && ip != "" {
			return ip
		}
		if e.Cfg.Monitoring == nil || time.Now().After(deadline) {
			return ""
		}
		select {
		case <-ctx.Done():
			return ""
		case <-time.After(memberIPPoll):
		}
	}
}

func (e *Engine) destroyMember(ctx context.Context, pool *state.Pool, vmid int) error {
	e.logf("pool %s: destroying member %d", pool.Name, vmid)
	memberOS, memberNode := "linux", ""
	if m, ok := pool.FindMember(vmid); ok {
		memberOS, memberNode = m.OS, m.PVENode
	}
	prov, err := e.providerForPool(pool.Name)
	if err != nil {
		return err
	}
	inst, err := prov.GetInstance(ctx, memberNode, vmid)
	if err != nil {
		// VM already gone (manual deletion) — just drop it from state.
		pool.RemoveMember(vmid)
		return nil
	}

	if running, _ := inst.IsRunning(ctx); running {
		// Best-effort drain; don't block destruction if ephemerd is wedged.
		_, _ = inst.Exec(ctx, drainCmd(memberOS), 330)
		_ = inst.Shutdown(ctx)
	}

	if err := inst.Delete(ctx); err != nil {
		return fmt.Errorf("wait for delete %d: %w", vmid, err)
	}

	pool.RemoveMember(vmid)
	return nil
}

func (e *Engine) destroyPool(ctx context.Context, name string) error {
	pool, ok := e.St.FindPool(name)
	if !ok {
		return fmt.Errorf("pool %s missing from state", name)
	}
	for len(pool.Members) > 0 {
		m := pool.Members[0]
		// Enrolled bare-metal members (darwin or linux) carry no VMID — the
		// hardware isn't ours to destroy, only unenroll. VM members have a
		// VMID and are torn down in PVE.
		if m.VMID == 0 {
			if err := e.unenrollMember(ctx, pool, m.Hostname); err != nil {
				return err
			}
		} else if err := e.destroyMember(ctx, pool, m.VMID); err != nil {
			return err
		}
		if err := e.St.Save(); err != nil {
			return err
		}
	}
	e.logf("pool %s: deleting tunnel + DNS", name)
	if err := e.CF.DeleteTunnel(ctx, pool.Hostname, pool.TunnelID); err != nil {
		return err
	}
	e.St.RemovePool(name)
	return e.St.Save()
}

// EphemerdSource applies a pool's ephemerd_version pin over the global
// [ephemerd] settings. A per-pool pin also clears any global binary_url
// override, since that URL encodes a specific version. Exported so
// `mayfly node upgrade` honors the same pin apply does.
func EphemerdSource(global config.Ephemerd, spec config.PoolSpec) config.Ephemerd {
	if spec.EphemerdVersion != "" {
		global.Version = spec.EphemerdVersion
		global.BinaryURL = ""
	}
	return global
}

// resolveEphemerdURL builds the release asset URL for the POOL's os/arch —
// never the machine running mayfly.
func resolveEphemerdURL(cfg config.Ephemerd, osName, arch string) (string, error) {
	if cfg.BinaryURL != "" {
		return cfg.BinaryURL, nil
	}
	if cfg.Version == "" {
		return "", fmt.Errorf("ephemerd.version or ephemerd.binary_url must be set in config")
	}
	base := fmt.Sprintf("https://github.com/ephpm/ephemerd/releases/download/%s/ephemerd_%s", cfg.Version, cfg.Version)
	switch osName {
	case "linux":
		return fmt.Sprintf("%s_linux_%s.tar.gz", base, arch), nil
	case "windows":
		if arch != "amd64" {
			return "", fmt.Errorf("no windows_%s release asset", arch)
		}
		return base + "_windows_amd64.zip", nil
	case "darwin":
		if arch != "arm64" {
			return "", fmt.Errorf("no darwin_%s release asset", arch)
		}
		return base + "_darwin_arm64.tar.gz", nil
	}
	return "", fmt.Errorf("unsupported os %q for ephemerd release naming", osName)
}

// pemPath is where the GitHub App private key lands inside the guest.
func pemPath(osName string) string {
	if osName == "windows" {
		return `C:\ProgramData\ephemerd\app.pem`
	}
	return "/etc/ephemerd/app.pem"
}

// configPath is where ephemerd reads its config inside the guest.
func configPath(osName string) string {
	if osName == "windows" {
		return `C:\ProgramData\ephemerd\config.toml`
	}
	return "/var/lib/ephemerd/config.toml"
}

// Guest-agent command lines per OS. The Windows service/binary layout comes
// from ephemerd's own installer (C:\Program Files\ephemerd).
const winExe = `C:\Program Files\ephemerd\ephemerd.exe`

func smokeCmd(osName string) []string {
	if osName == "windows" {
		return []string{"cmd.exe", "/c", "ver"}
	}
	return []string{"/bin/true"}
}

func drainCmd(osName string) []string {
	return EphemerdArgv(osName, "drain")
}

// restartCmd drains then restarts ephemerd (config-tier update).
func restartCmd(osName string) []string {
	if osName == "windows" {
		// `ephemerd restart` tries to signal the running process, which Windows
		// doesn't support (then `sc start` fails 1056: already running). Drain,
		// then bounce the Windows service directly.
		//
		// `ephemerd drain` makes the daemon exit once drained, so on an idle
		// node the service is already Stopped by the time the next command runs.
		// Restart-Service -Force then fails ("cannot be stopped") because it
		// tries to stop an already-stopped service — and crucially never starts
		// it, leaving the node down. Even a plain Stop-then-Start races: Start
		// fires before the exiting daemon releases its webhook port, so it fails.
		// Wait for Stopped, then retry Start (verified on metal: first retry
		// succeeds once the port is free).
		//
		// THE STOP WAIT MUST OUTLAST THE DRAIN. drain returns immediately and
		// the daemon exits only once its jobs finish, which ephemerd allows up
		// to 5 minutes for. A 30s wait was fine on an idle node and wrong on a
		// busy one: on 2026-09-21 a node with one live job sat in StopPending
		// for ~4 minutes, so the wait expired, all six Start-Service attempts
		// failed within ~18s (a service that is STOPPING cannot be started),
		// and this threw — after which the drain completed and the service
		// stayed Stopped. The apply reported failure and the node was down.
		// 8 minutes clears ephemerd's own ceiling with room to spare.
		//
		// The short retry loop stays: it exists for the port-release race,
		// which resolves in seconds. It was never capable of waiting out a
		// drain, and lengthening it instead of the stop wait would just spend
		// longer failing for the same reason.
		//
		// The last-ditch start is unconditional. Throwing while the service is
		// down turns a slow drain into an outage, so if the wait or the retries
		// give up, try once more and only then report — a node that came back
		// on its old config is a drift to fix next apply, not a dead runner.
		return []string{"powershell", "-NoProfile", "-Command",
			fmt.Sprintf("& '%s' drain; Stop-Service ephemerd -Force -ErrorAction SilentlyContinue; "+
				"try { (Get-Service ephemerd).WaitForStatus('Stopped','00:08:00') } catch {}; "+
				"$ok=$false; for ($i=0; $i -lt 6; $i++) { try { Start-Service ephemerd -ErrorAction Stop; $ok=$true; break } "+
				"catch { Start-Sleep -Seconds 3 } }; "+
				"if (-not $ok) { try { Start-Service ephemerd -ErrorAction Stop; $ok=$true } catch {} }; "+
				"if (-not $ok) { throw 'ephemerd failed to start after retries' }; "+
				"(Get-Service ephemerd).WaitForStatus('Running','00:00:30')", winExe)}
	}
	// enable so the service survives a reboot (older nodes were only started).
	return []string{"/bin/sh", "-c", "/usr/local/bin/ephemerd drain || true; systemctl enable ephemerd; systemctl restart ephemerd"}
}

// growCmd expands the root filesystem after a live disk resize.
func growCmd(osName string) []string {
	if osName == "windows" {
		// Idempotent: only resize when there's headroom. Resize-Partition errors
		// ("already the requested size") if C: is already at max — which would
		// fail the whole update and prevent state from being recorded on re-apply.
		//
		// A trailing recovery partition (disk0 part2) can sit between C: and the
		// new free space and block the extend, so first delete any partition
		// after C: that carries no drive letter. Mirrors the first-boot
		// winsetup.ps1 reconcile in the Windows cloud-init user-data.
		return []string{"powershell", "-NoProfile", "-Command",
			`Update-Disk -Number 0; $c = Get-Partition -DriveLetter C; ` +
				`Get-Partition -DiskNumber $c.DiskNumber | Where-Object { $_.Offset -gt $c.Offset -and -not $_.DriveLetter } | ForEach-Object { Remove-Partition -DiskNumber $_.DiskNumber -PartitionNumber $_.PartitionNumber -Confirm:$false }; ` +
				`$max = (Get-PartitionSupportedSize -DriveLetter C).SizeMax; if ($max -gt (Get-Partition -DriveLetter C).Size) { Resize-Partition -DriveLetter C -Size $max }`}
	}
	return []string{"/bin/sh", "-c", "growpart /dev/sda 1 && resize2fs /dev/sda1"}
}
