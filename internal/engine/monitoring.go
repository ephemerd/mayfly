package engine

import (
	"context"
	"encoding/base64"
	"fmt"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/luthermonson/mayfly/internal/cloudinit"
	"github.com/luthermonson/mayfly/internal/config"
	"github.com/luthermonson/mayfly/internal/monitoring"
	"github.com/luthermonson/mayfly/internal/provider"
	"github.com/luthermonson/mayfly/internal/pve"
	"github.com/luthermonson/mayfly/internal/registrycache"
	"github.com/luthermonson/mayfly/internal/state"
)

// monitoringTag marks the observability VM so it is easy to spot in PVE.
const monitoringTag = "mayfly-monitoring"

// monitoringFiles assembles the cloud-init file drop: the embedded stack
// assets, the Grafana admin .env, the initial Prometheus target list
// (generated from current state), and — when enabled — the registry
// pull-through cache's compose project, .env, prune script, and systemd units.
func (e *Engine) monitoringFiles(spec config.MonitoringSpec, cache config.RegistryCacheSpec) ([]cloudinit.WriteFile, error) {
	stack, err := monitoring.StackFiles()
	if err != nil {
		return nil, err
	}
	files := make([]cloudinit.WriteFile, 0, len(stack)+2)
	for _, f := range stack {
		files = append(files, cloudinit.WriteFile{
			Path:          f.Path,
			Mode:          f.Mode,
			ContentBase64: base64.StdEncoding.EncodeToString(f.Content),
		})
	}

	pw := spec.GrafanaAdminPassword
	if pw == "" {
		pw = "admin"
	}
	env := fmt.Sprintf("GRAFANA_ADMIN_PASSWORD=%s\nPROM_RETENTION=%dd\n", pw, spec.RetentionDays)
	files = append(files, cloudinit.WriteFile{
		Path:          monitoring.EnvPath,
		Mode:          "0600",
		ContentBase64: base64.StdEncoding.EncodeToString([]byte(env)),
	})

	targets, err := monitoring.TargetsJSON(e.St, spec.MetricsPort)
	if err != nil {
		return nil, err
	}
	files = append(files, cloudinit.WriteFile{
		Path:          monitoring.TargetsPath,
		Mode:          "0644",
		ContentBase64: base64.StdEncoding.EncodeToString(targets),
	})

	cacheFiles, err := registryCacheFiles(cache)
	if err != nil {
		return nil, err
	}
	return append(files, cacheFiles...), nil
}

// registryCacheFiles renders the pull-through cache's on-VM files. Empty when
// the cache is disabled, so a fleet without one gets byte-identical output to
// what mayfly produced before the cache existed.
func registryCacheFiles(cache config.RegistryCacheSpec) ([]cloudinit.WriteFile, error) {
	if !cache.Enabled {
		return nil, nil
	}
	stack, err := registrycache.StackFiles()
	if err != nil {
		return nil, err
	}
	out := make([]cloudinit.WriteFile, 0, len(stack)+1)
	for _, f := range stack {
		out = append(out, cloudinit.WriteFile{
			Path:          f.Path,
			Mode:          f.Mode,
			ContentBase64: base64.StdEncoding.EncodeToString(f.Content),
		})
	}
	r := cache.Resolved()
	env := registrycache.EnvFile(registrycache.Options{
		Port:      r.Port,
		Upstream:  r.Upstream,
		TTL:       r.TTL,
		MaxSizeGB: r.MaxSizeGB,
		Username:  r.Username,
		Password:  r.Password,
	})
	// 0600: may carry upstream registry credentials.
	out = append(out, cloudinit.WriteFile{
		Path:          registrycache.EnvPath,
		Mode:          "0600",
		ContentBase64: base64.StdEncoding.EncodeToString(env),
	})
	return out, nil
}

// utilityDirs returns every directory the utility host needs created before
// files land — the monitoring stack's, plus the cache's when enabled.
func utilityDirs(cache config.RegistryCacheSpec) []string {
	dirs := monitoring.StackDirs()
	if cache.Enabled {
		dirs = append(dirs, registrycache.StackDirs()...)
	}
	return dirs
}

// utilityComposeDirs returns the Compose projects to bring up on first boot.
// The cache is a second, independent project so its lifecycle never disturbs
// Prometheus or Grafana.
func utilityComposeDirs(cache config.RegistryCacheSpec) []string {
	dirs := []string{monitoring.ComposeDir}
	if cache.Enabled {
		dirs = append(dirs, registrycache.ComposeDir)
	}
	return dirs
}

// utilityUnits returns the systemd units to enable on first boot.
func utilityUnits(cache config.RegistryCacheSpec) []string {
	if !cache.Enabled {
		return nil
	}
	return []string{registrycache.PruneTimer}
}

func (e *Engine) provisionMonitoring(ctx context.Context, spec config.MonitoringSpec, cache config.RegistryCacheSpec) error {
	prov, err := e.monitoringProvider()
	if err != nil {
		return err
	}
	vmid, err := prov.NextID(ctx)
	if err != nil {
		return fmt.Errorf("next vmid: %w", err)
	}
	name := fmt.Sprintf("mfl-monitoring-%d", vmid)
	pveNode, pveStorage := spec.ResolvedPlacement(e.Cfg.Proxmox)
	e.logf("monitoring: provisioning %s (vmid %d on %s)", name, vmid, pveNode)

	files, err := e.monitoringFiles(spec, cache)
	if err != nil {
		return err
	}
	rendered, err := cloudinit.RenderMonitoring(cloudinit.MonitoringInputs{
		Hostname:    name,
		Dirs:        utilityDirs(cache),
		Files:       files,
		ComposeDirs: utilityComposeDirs(cache),
		Units:       utilityUnits(cache),
	})
	if err != nil {
		return err
	}

	inst, err := prov.CreateInstance(ctx, provider.CreateSpec{
		ID:               vmid,
		Name:             name,
		Template:         spec.Template,
		Node:             pveNode,
		Storage:          pveStorage,
		Arch:             "amd64",
		Cores:            spec.Cores,
		MemoryMB:         spec.MemoryMB,
		DiskGB:           spec.DiskGB,
		Tags:             []string{pve.TagMayfly, monitoringTag},
		UserData:         rendered.UserData,
		MetaData:         rendered.MetaData,
		CloudInitStorage: "", // monitoring keeps its cloud-init ISO on the disk storage
	})
	if err != nil {
		return err
	}

	if err := inst.Start(ctx); err != nil {
		return err
	}

	e.logf("monitoring: waiting for guest agent on %s", name)
	if err := inst.WaitForReady(ctx, 180); err != nil {
		return fmt.Errorf("guest agent: %w", err)
	}
	ip, _ := inst.PrimaryIPv4(ctx)

	e.St.Monitoring = &state.Monitoring{
		VMID:       vmid,
		Hostname:   name,
		PVENode:    pveNode,
		IP:         ip,
		Cores:      spec.Cores,
		MemoryMB:   spec.MemoryMB,
		DiskGB:     spec.DiskGB,
		Template:   spec.Template,
		ConfigHash: MonitoringConfigHash(spec, cache),
		TargetHash: monitoring.TargetsHash(e.St, spec.MetricsPort),
		CreatedAt:  time.Now().UTC(),
	}
	if ip != "" {
		e.logf("monitoring: Grafana at http://%s:3000 (admin) — Prometheus scraping %d target(s)",
			ip, len(monitoring.BuildTargets(e.St, spec.MetricsPort)))
		if cache.Enabled {
			r := cache.Resolved()
			e.logf("monitoring: registry pull-through cache at http://%s:%d → %s (cap %dGB)",
				ip, r.Port, r.Upstream, r.MaxSizeGB)
		}
	}
	return e.St.Save()
}

func (e *Engine) updateMonitoring(ctx context.Context, u *MonitoringUpdate) error {
	cur := e.St.Monitoring
	if cur == nil {
		return fmt.Errorf("monitoring missing from state")
	}
	prov, err := e.monitoringProvider()
	if err != nil {
		return err
	}
	inst, err := prov.GetInstance(ctx, cur.PVENode, cur.VMID)
	if err != nil {
		return fmt.Errorf("look up monitoring vm: %w", err)
	}

	if u.PushConfig {
		e.logf("monitoring: pushing stack config + recomposing")
		files, err := e.monitoringFiles(u.Spec, u.Cache)
		if err != nil {
			return err
		}
		// Create every parent directory before writing. On the CREATE path
		// cloud-init's write_files makes parents implicitly, so this was
		// invisible until a feature added a file in a directory the create
		// path had already made — the registry cache's /opt/registry-cache,
		// which exists on hosts provisioned with the cache but not on one
		// that predates it. Enabling the cache on an existing utility host
		// then failed with "No such file or directory".
		if err := e.guestExec(ctx, inst, mkdirCmd(guestParentDirs(files)), 120); err != nil {
			return fmt.Errorf("create stack directories: %w", err)
		}
		for _, f := range files {
			content, err := base64.StdEncoding.DecodeString(f.ContentBase64)
			if err != nil {
				return err
			}
			if err := inst.FileWrite(ctx, f.Path, content); err != nil {
				return fmt.Errorf("write %s: %w", f.Path, err)
			}
		}
		if err := e.guestExec(ctx, inst, utilityReconcileCmd(u.Cache), 600); err != nil {
			return fmt.Errorf("reconcile utility host: %w", err)
		}
		cur.ConfigHash = MonitoringConfigHash(u.Spec, u.Cache)
		cur.TargetHash = monitoring.TargetsHash(e.St, u.Spec.MetricsPort)
	} else if u.PushTargets {
		e.logf("monitoring: refreshing scrape targets")
		targets, err := monitoring.TargetsJSON(e.St, u.Spec.MetricsPort)
		if err != nil {
			return err
		}
		if err := inst.FileWrite(ctx, monitoring.TargetsPath, targets); err != nil {
			return fmt.Errorf("write targets: %w", err)
		}
		cur.TargetHash = monitoring.TargetsHash(e.St, u.Spec.MetricsPort)
	}

	if u.PowerCycle {
		e.logf("monitoring: power cycling (cores %d→%d, mem %d→%dMB)",
			u.FromCores, u.Spec.Cores, u.FromMemMB, u.Spec.MemoryMB)
		if err := inst.Shutdown(ctx); err != nil {
			return err
		}
		if err := inst.SetResources(ctx, u.Spec.Cores, u.Spec.MemoryMB); err != nil {
			return err
		}
		if err := inst.Start(ctx); err != nil {
			return err
		}
		if err := inst.WaitForReady(ctx, 180); err != nil {
			return fmt.Errorf("guest agent after cycle: %w", err)
		}
		cur.Cores = u.Spec.Cores
		cur.MemoryMB = u.Spec.MemoryMB
	}

	if u.DiskGrowGB > 0 {
		e.logf("monitoring: growing disk %dG→%dG (live)", u.FromDiskGB, u.DiskGrowGB)
		if err := inst.ResizeDisk(ctx, u.DiskGrowGB); err != nil {
			return err
		}
		if err := e.guestExec(ctx, inst, growCmd("linux"), 120); err != nil {
			return fmt.Errorf("grow filesystem: %w", err)
		}
		cur.DiskGB = u.DiskGrowGB
	}

	return e.St.Save()
}

func (e *Engine) destroyMonitoring(ctx context.Context) error {
	cur := e.St.Monitoring
	if cur == nil {
		return nil
	}
	e.logf("monitoring: destroying node (vmid %d)", cur.VMID)
	prov, err := e.monitoringProvider()
	if err != nil {
		return err
	}
	inst, err := prov.GetInstance(ctx, cur.PVENode, cur.VMID)
	if err != nil {
		// VM already gone — just clear state.
		e.St.Monitoring = nil
		return e.St.Save()
	}
	if running, _ := inst.IsRunning(ctx); running {
		if err := inst.Shutdown(ctx); err != nil {
			return err
		}
	}
	if err := inst.Delete(ctx); err != nil {
		return fmt.Errorf("delete monitoring vm %d: %w", cur.VMID, err)
	}
	if cur.TunnelID != "" {
		if err := e.CF.DeleteTunnel(ctx, cur.Hostname, cur.TunnelID); err != nil {
			e.logf("monitoring: warning: could not delete tunnel: %v", err)
		}
	}
	e.St.Monitoring = nil
	return e.St.Save()
}

// utilityReconcileCmd re-applies the utility host's services after a config
// push. It is idempotent by construction — every step is a converge, not a
// create — so re-running apply never duplicates or breaks anything.
//
// When the cache is enabled: ensure its blob-store dir and script mode (the
// guest-agent file write carries no mode), bring its Compose project up, and
// enable the prune timer. When disabled: stop the project, drop the timer, and
// reclaim the blob store — a pull-through cache holds nothing that isn't
// re-fetchable, and leaving tens of GB behind on a host with a disk-fill
// history would be the wrong default.
// guestParentDirs returns the deduplicated, sorted parent directories of a
// file drop. Sorted so the command is deterministic and diffable in logs.
func guestParentDirs(files []cloudinit.WriteFile) []string {
	seen := map[string]bool{}
	for _, f := range files {
		// Guest paths are always POSIX, regardless of the host mayfly runs
		// on — path.Dir, never filepath.Dir, which would split on "\" when
		// mayfly runs from Windows and yield "." for every path.
		d := path.Dir(f.Path)
		if d == "" || d == "." || d == "/" {
			continue
		}
		seen[d] = true
	}
	dirs := make([]string, 0, len(seen))
	for d := range seen {
		dirs = append(dirs, d)
	}
	sort.Strings(dirs)
	return dirs
}

// mkdirCmd builds an idempotent `install -d` for each directory. Returns a
// harmless no-op when there is nothing to create, so callers need no guard.
func mkdirCmd(dirs []string) []string {
	if len(dirs) == 0 {
		return []string{"/bin/sh", "-c", "true"}
	}
	var b strings.Builder
	b.WriteString("set -e")
	for _, d := range dirs {
		b.WriteString("; install -d -m 0755 '" + d + "'")
	}
	return []string{"/bin/sh", "-c", b.String()}
}

func utilityReconcileCmd(cache config.RegistryCacheSpec) []string {
	var b strings.Builder
	b.WriteString("set -e; cd " + monitoring.ComposeDir + " && docker compose up -d")
	if cache.Enabled {
		b.WriteString("; install -d -m 0755 " + registrycache.DataDir)
		b.WriteString("; chmod 0755 " + registrycache.PruneScriptPath)
		b.WriteString("; cd " + registrycache.ComposeDir + " && docker compose up -d")
		b.WriteString("; systemctl daemon-reload; systemctl enable --now " + registrycache.PruneTimer)
	} else {
		b.WriteString("; systemctl disable --now " + registrycache.PruneTimer + " >/dev/null 2>&1 || true")
		b.WriteString("; if [ -f " + registrycache.ComposeDir + "/docker-compose.yml ]; then cd " +
			registrycache.ComposeDir + " && docker compose down --remove-orphans || true; fi")
		b.WriteString("; rm -rf " + registrycache.DataDir + "/docker")
	}
	return []string{"/bin/sh", "-c", b.String()}
}
