package engine

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/luthermonson/mayfly/internal/cloudinit"
	"github.com/luthermonson/mayfly/internal/config"
	"github.com/luthermonson/mayfly/internal/provider"
	"github.com/luthermonson/mayfly/internal/pve"
	"github.com/luthermonson/mayfly/internal/registrycache"
	"github.com/luthermonson/mayfly/internal/state"
)

// registryCacheTag marks the dedicated cache VM in Proxmox, the same way
// monitoringTag marks the utility host.
const registryCacheTag = "mayfly-registry-cache"

// RegistryCacheConfigHash fingerprints the dedicated cache VM's stack: the
// embedded compose/prune assets plus the spec fields that shape the rendered
// .env. Resource fields (cores/mem/disk) and the VLAN tag are compared as
// plain values rather than folded in, matching how the monitoring host is
// handled — a hash match must mean "the files on disk are right", nothing more.
func RegistryCacheConfigHash(cache config.RegistryCacheSpec) string {
	h := sha256.New()
	c := cache.Resolved()
	fmt.Fprintf(h, "registry_cache_vm assets=%s port=%d upstream=%s ttl=%s max_gb=%d user=%s pass=%s\n",
		registrycache.AssetsHash(), c.Port, c.Upstream, c.TTL, c.MaxSizeGB, c.Username, c.Password)
	// Appended only when on, so a cache VM without the Actions registry keeps
	// the hash it had before this existed and sees no drift.
	if cache.ActionsActive() {
		a := cache.Actions.Resolved()
		fmt.Fprintf(h, "actions assets=%s port=%d max_gb=%d cred=%s\n",
			registrycache.ActionsAssetsHash(), a.Port, a.MaxSizeGB,
			registrycache.ActionsCredentialFingerprint(a.Username, a.ResolvedPassword()))
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// registryCacheVMFiles renders the cache VM's on-disk assets: the same
// pull-through stack the utility host gets when the cache is colocated, plus
// the writable Actions registry when [registry_cache.actions] is on.
func registryCacheVMFiles(cache config.RegistryCacheSpec) ([]cloudinit.WriteFile, error) {
	out, err := registryCacheFiles(cache)
	if err != nil || !cache.ActionsActive() {
		return out, err
	}
	stack, err := registrycache.ActionsStackFiles()
	if err != nil {
		return nil, err
	}
	for _, f := range stack {
		out = append(out, cloudinit.WriteFile{
			Path:          f.Path,
			Mode:          f.Mode,
			ContentBase64: base64.StdEncoding.EncodeToString(f.Content),
		})
	}
	a := cache.Actions.Resolved()
	htpasswd, err := registrycache.ActionsHtpasswd(a.Username, a.ResolvedPassword())
	if err != nil {
		return nil, err
	}
	env := registrycache.ActionsEnvFile(registrycache.ActionsOptions{Port: a.Port, MaxSizeGB: a.MaxSizeGB})
	out = append(out,
		cloudinit.WriteFile{
			Path:          registrycache.ActionsEnvPath,
			Mode:          "0644",
			ContentBase64: base64.StdEncoding.EncodeToString(env),
		},
		// 0644, not 0600: the registry container reads it through a bind
		// mount, and it holds only a bcrypt hash.
		cloudinit.WriteFile{
			Path:          registrycache.ActionsHtpasswdPath,
			Mode:          "0644",
			ContentBase64: base64.StdEncoding.EncodeToString(htpasswd),
		},
	)
	return out, nil
}

// registryCacheVMDirs, ...ComposeDirs and ...Units are what the cache VM's
// cloud-init creates, brings up and enables.
func registryCacheVMDirs(cache config.RegistryCacheSpec) []string {
	dirs := registrycache.StackDirs()
	if cache.ActionsActive() {
		dirs = append(dirs, registrycache.ActionsStackDirs()...)
	}
	return dirs
}

func registryCacheVMComposeDirs(cache config.RegistryCacheSpec) []string {
	dirs := []string{registrycache.ComposeDir}
	if cache.ActionsActive() {
		dirs = append(dirs, registrycache.ActionsComposeDir)
	}
	return dirs
}

func registryCacheVMUnits(cache config.RegistryCacheSpec) []string {
	units := []string{registrycache.PruneTimer}
	if cache.ActionsActive() {
		units = append(units, registrycache.ActionsGCTimer)
	}
	return units
}

// registryCacheVMReconcileCmd re-applies the cache VM's services after a
// config push. It is the dedicated VM's own command, not the utility host's:
// there is no monitoring project on this VM to bring up. Turning the Actions
// registry off stops it and removes its blobs.
func registryCacheVMReconcileCmd(cache config.RegistryCacheSpec) []string {
	var b strings.Builder
	b.WriteString("set -e")
	b.WriteString("; install -d -m 0755 " + registrycache.DataDir)
	b.WriteString("; chmod 0755 " + registrycache.PruneScriptPath)
	b.WriteString("; cd " + registrycache.ComposeDir + " && docker compose up -d")
	b.WriteString("; systemctl daemon-reload; systemctl enable --now " + registrycache.PruneTimer)
	if cache.ActionsActive() {
		b.WriteString("; install -d -m 0755 " + registrycache.ActionsDataDir)
		b.WriteString("; chmod 0755 " + registrycache.ActionsGCScriptPath)
		// --force-recreate: compose does not notice a changed htpasswd behind
		// an unchanged service definition, and a rotated password must take.
		b.WriteString("; cd " + registrycache.ActionsComposeDir + " && docker compose up -d --force-recreate")
		b.WriteString("; systemctl enable --now " + registrycache.ActionsGCTimer)
	} else {
		b.WriteString("; systemctl disable --now " + registrycache.ActionsGCTimer + " >/dev/null 2>&1 || true")
		b.WriteString("; if [ -f " + registrycache.ActionsComposeDir + "/docker-compose.yml ]; then cd " +
			registrycache.ActionsComposeDir + " && docker compose down --remove-orphans || true; fi")
		b.WriteString("; rm -rf " + registrycache.ActionsDataDir + "/docker")
	}
	return []string{"/bin/sh", "-c", b.String()}
}

// provisionRegistryCacheVM creates the dedicated cache VM and brings the
// pull-through cache up on it.
//
// The VLAN tag is the reason this code path exists: the cache has to be
// reachable from the runners, and the runners live on their own VLAN. See
// RegistryCacheSpec.VM for the full rationale.
func (e *Engine) provisionRegistryCacheVM(ctx context.Context, cache config.RegistryCacheSpec) error {
	if !cache.Dedicated() {
		return nil
	}
	vm := cache.VM.Resolved()
	prov, err := e.monitoringProvider()
	if err != nil {
		return err
	}
	vmid, err := prov.NextID(ctx)
	if err != nil {
		return fmt.Errorf("next vmid: %w", err)
	}
	name := fmt.Sprintf("mfl-registry-cache-%d", vmid)
	pveNode, pveStorage := vm.ResolvedPlacement(e.Cfg.Proxmox)
	e.logf("registry-cache: provisioning %s (vmid %d on %s, vlan %d)", name, vmid, pveNode, vm.VLANTag)

	files, err := registryCacheVMFiles(cache)
	if err != nil {
		return err
	}
	// Reuses the utility-host renderer: it installs Docker, drops the files,
	// brings the Compose projects up and enables the units. A cache-only host
	// simply passes the cache's dirs/files/units and nothing else.
	rendered, err := cloudinit.RenderMonitoring(cloudinit.MonitoringInputs{
		Hostname:    name,
		Dirs:        registryCacheVMDirs(cache),
		Files:       files,
		ComposeDirs: registryCacheVMComposeDirs(cache),
		Units:       registryCacheVMUnits(cache),
	})
	if err != nil {
		return err
	}

	inst, err := prov.CreateInstance(ctx, provider.CreateSpec{
		ID:               vmid,
		Name:             name,
		Template:         vm.Template,
		Node:             pveNode,
		Storage:          pveStorage,
		Arch:             "amd64",
		Cores:            vm.Cores,
		MemoryMB:         vm.MemoryMB,
		DiskGB:           vm.DiskGB,
		Tags:             []string{pve.TagMayfly, registryCacheTag},
		VLANTag:          vm.VLANTag,
		VMStorage:        vm.VMStorage,
		Firewall:         vm.Firewall,
		UserData:         rendered.UserData,
		MetaData:         rendered.MetaData,
		CloudInitStorage: "", // same as monitoring: ISO on the disk storage
	})
	if err != nil {
		return err
	}
	if err := inst.Start(ctx); err != nil {
		return err
	}

	e.logf("registry-cache: waiting for guest agent on %s", name)
	if err := inst.WaitForReady(ctx, 180); err != nil {
		return fmt.Errorf("guest agent: %w", err)
	}
	ip, _ := inst.PrimaryIPv4(ctx)

	e.St.RegistryCache = &state.RegistryCache{
		VMID:       vmid,
		Hostname:   name,
		PVENode:    pveNode,
		IP:         ip,
		Cores:      vm.Cores,
		MemoryMB:   vm.MemoryMB,
		DiskGB:     vm.DiskGB,
		Template:   vm.Template,
		VLANTag:    vm.VLANTag,
		VMStorage:  vm.VMStorage,
		ConfigHash: RegistryCacheConfigHash(cache),
		CreatedAt:  time.Now().UTC(),
	}
	if ip != "" {
		r := cache.Resolved()
		e.logf("registry-cache: pull-through cache at http://%s:%d → %s (cap %dGB)", ip, r.Port, r.Upstream, r.MaxSizeGB)
	} else {
		// Without an address there is no mirror endpoint to hand the runners,
		// so say so plainly rather than let it look provisioned-and-working.
		e.logf("registry-cache: warning: no IPv4 from the guest agent yet — runners get no mirror until the next apply picks the address up")
	}
	return e.St.Save()
}

// updateRegistryCacheVM re-pushes the cache stack in place and grows the disk.
// Resource changes that need a power cycle are handled the same way the
// utility host handles them: cores/memory are applied cold.
func (e *Engine) updateRegistryCacheVM(ctx context.Context, u *RegistryCacheUpdate) error {
	cur := e.St.RegistryCache
	if cur == nil {
		return nil
	}
	prov, err := e.monitoringProvider()
	if err != nil {
		return err
	}
	inst, err := prov.GetInstance(ctx, cur.PVENode, cur.VMID)
	if err != nil {
		return fmt.Errorf("registry-cache vm %d: %w", cur.VMID, err)
	}
	vm := u.Cache.VM.Resolved()

	if u.PushConfig {
		e.logf("registry-cache: pushing stack config + recomposing")
		files, err := registryCacheVMFiles(u.Cache)
		if err != nil {
			return err
		}
		// Same parent-directory precaution as the utility host: the guest-agent
		// write path does not create parents, unlike cloud-init's write_files
		// on the create path.
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
		if err := e.guestExec(ctx, inst, registryCacheVMReconcileCmd(u.Cache), 600); err != nil {
			return fmt.Errorf("reconcile registry-cache host: %w", err)
		}
		cur.ConfigHash = RegistryCacheConfigHash(u.Cache)
	}

	if u.PowerCycle {
		e.logf("registry-cache: power cycling (cores %d→%d, mem %d→%dMB)",
			u.FromCores, vm.Cores, u.FromMemMB, vm.MemoryMB)
		if err := inst.Shutdown(ctx); err != nil {
			return err
		}
		if err := inst.SetResources(ctx, vm.Cores, vm.MemoryMB); err != nil {
			return err
		}
		if err := inst.Start(ctx); err != nil {
			return err
		}
		if err := inst.WaitForReady(ctx, 180); err != nil {
			return fmt.Errorf("guest agent after cycle: %w", err)
		}
		cur.Cores, cur.MemoryMB = vm.Cores, vm.MemoryMB
	}

	// Move before growing, so the grow lands on the new (bigger) storage. A
	// live move keeps the VM, its address and its blobs — a recreate would
	// lose the warm caches and hand the runners a new IP.
	//
	// Any move failure returns BEFORE the grow: growing the disk on the old
	// storage is the one outcome to avoid (on node-b that is the `local` dir on
	// the host's root filesystem).
	if u.MoveDiskTo != "" {
		if err := e.moveRegistryCacheDisk(ctx, inst, u.MoveDiskTo); err != nil {
			return err
		}
		cur.VMStorage = u.MoveDiskTo
		// Saved now, so a failed grow below cannot make the next apply
		// forget the disk already moved.
		if err := e.St.Save(); err != nil {
			return err
		}
	}
	if u.DiskGrowGB > 0 {
		e.logf("registry-cache: growing disk %dG→%dG (live)", u.FromDiskGB, u.DiskGrowGB)
		if err := inst.ResizeDisk(ctx, u.DiskGrowGB); err != nil {
			return err
		}
		if err := e.guestExec(ctx, inst, growCmd("linux"), 120); err != nil {
			return fmt.Errorf("grow filesystem: %w", err)
		}
		cur.DiskGB = u.DiskGrowGB
	}
	// Refresh the address every update: the runners' mirror endpoint is
	// derived from it, and a DHCP lease can move.
	if ip, err := inst.PrimaryIPv4(ctx); err == nil && ip != "" {
		cur.IP = ip
	}
	return e.St.Save()
}

// moveRegistryCacheDisk moves the cache VM's disk onto target, recovering from
// a move an earlier apply started but stopped waiting for.
//
// A large disk can outlast mayfly's wait while Proxmox keeps copying. The
// disk's live storage is checked first, so:
//   - already on target: that earlier move finished — record it, no second move;
//   - VM config locked: an operation is still in progress — say so, don't stack
//     another move on top of it;
//   - move outlives the wait now: report it as still running, with what to
//     check, and leave state untouched so the next apply re-checks.
func (e *Engine) moveRegistryCacheDisk(ctx context.Context, inst provider.Instance, target string) error {
	vmid := inst.ID()
	storage, lock, err := inst.DiskStorage(ctx)
	switch {
	case err != nil:
		return fmt.Errorf("registry-cache: reading vm %d's disk storage before the move: %w", vmid, err)
	case storage == target:
		e.logf("registry-cache: disk is already on %s (an earlier move finished); recording it", target)
		return nil
	case lock != "":
		return fmt.Errorf(
			"registry-cache: vm %d is locked (%q), most likely by a disk move still copying onto %s — "+
				"check the node's task log (Datacenter → Tasks, or `qm config %d | grep -E 'lock|scsi0'`), wait for it to finish, then re-run apply",
			vmid, lock, target, vmid)
	}
	e.logf("registry-cache: moving disk %s→%s (live)", storage, target)
	if err := inst.MoveDisk(ctx, target); err != nil {
		if errors.Is(err, provider.ErrTaskStillRunning) {
			return fmt.Errorf(
				"registry-cache: vm %d's disk move to %s is still running in Proxmox after mayfly's wait: %w — "+
					"nothing was grown and state is unchanged; when the task finishes, re-run apply and it will record the move "+
					"(or retry it if the task failed)",
				vmid, target, err)
		}
		return fmt.Errorf("registry-cache: moving vm %d's disk to %s failed: %w — the disk stays on %s, nothing was grown", vmid, target, err, storage)
	}
	return nil
}

// destroyRegistryCacheVM removes the dedicated cache VM. The blob store goes
// with it, which is fine: a pull-through cache holds nothing that is not
// re-fetchable from upstream.
func (e *Engine) destroyRegistryCacheVM(ctx context.Context) error {
	cur := e.St.RegistryCache
	if cur == nil {
		return nil
	}
	e.logf("registry-cache: destroying vm (vmid %d)", cur.VMID)
	prov, err := e.monitoringProvider()
	if err != nil {
		return err
	}
	inst, err := prov.GetInstance(ctx, cur.PVENode, cur.VMID)
	if err != nil {
		// Already gone — just clear state.
		e.St.RegistryCache = nil
		return e.St.Save()
	}
	if running, _ := inst.IsRunning(ctx); running {
		if err := inst.Shutdown(ctx); err != nil {
			return err
		}
	}
	if err := inst.Delete(ctx); err != nil {
		return fmt.Errorf("delete registry-cache vm %d: %w", cur.VMID, err)
	}
	e.St.RegistryCache = nil
	return e.St.Save()
}
