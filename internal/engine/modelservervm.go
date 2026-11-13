package engine

import (
	"context"
	"encoding/base64"
	"fmt"
	"slices"
	"time"

	"github.com/ephemerd/mayfly/internal/cloudinit"
	"github.com/ephemerd/mayfly/internal/config"
	"github.com/ephemerd/mayfly/internal/modelserver"
	"github.com/ephemerd/mayfly/internal/provider"
	"github.com/ephemerd/mayfly/internal/pve"
	"github.com/ephemerd/mayfly/internal/state"
)

// modelServerTag marks the model-server VM in Proxmox.
const modelServerTag = "mayfly-model-server"

func modelServerSpec(m config.ModelServerSpec) modelserver.Spec {
	r := m.Resolved()
	return modelserver.Spec{Port: r.Port, Models: r.Models}
}

// ModelServerConfigHash fingerprints the server's guest setup. Resource
// fields and the passthrough set are compared as plain values in the plan,
// as for the registry cache VM.
func ModelServerConfigHash(m config.ModelServerSpec) string {
	return modelserver.Hash(modelServerSpec(m))
}

// modelServerFiles renders the guest files as cloud-init write_files.
func modelServerFiles(m config.ModelServerSpec) []cloudinit.WriteFile {
	var out []cloudinit.WriteFile
	for _, f := range modelserver.Files(modelServerSpec(m)) {
		out = append(out, cloudinit.WriteFile{
			Path:          f.Path,
			Mode:          f.Mode,
			ContentBase64: base64.StdEncoding.EncodeToString(f.Content),
		})
	}
	return out
}

// hostPCIDevices turns the config's passthrough list into provider devices.
// An address passes the device on the plain PCI bus; anything else is a
// resource-mapping name (validation has already rejected the rest).
func hostPCIDevices(entries []string) []provider.HostPCI {
	devs := make([]provider.HostPCI, 0, len(entries))
	for _, e := range entries {
		if config.IsPCIAddress(e) {
			devs = append(devs, provider.HostPCI{Host: e})
		} else {
			devs = append(devs, provider.HostPCI{Mapping: e})
		}
	}
	return devs
}

// provisionModelServerVM creates the model-server VM with its GPU passed
// through and starts it. Provisioning returns once the guest agent answers;
// the driver install, Ollama install and model pull carry on inside the
// guest, so the endpoint comes up a few minutes after apply finishes.
func (e *Engine) provisionModelServerVM(ctx context.Context, m config.ModelServerSpec) error {
	if !m.Active() {
		return nil
	}
	vm := m.VM.Resolved()
	prov, err := e.monitoringProvider()
	if err != nil {
		return err
	}
	vmid, err := prov.NextID(ctx)
	if err != nil {
		return fmt.Errorf("next vmid: %w", err)
	}
	name := fmt.Sprintf("mfl-model-server-%d", vmid)
	pveNode, pveStorage := vm.ResolvedPlacement(e.Cfg.Proxmox)
	e.logf("model-server: provisioning %s (vmid %d on %s, vlan %d, hostpci %v)", name, vmid, pveNode, vm.VLANTag, vm.HostPCI)

	rendered, err := cloudinit.RenderModelServer(cloudinit.ModelServerInputs{
		Hostname: name,
		Files:    modelServerFiles(m),
		Commands: modelserver.FirstBootCommands(),
	})
	if err != nil {
		return err
	}

	inst, err := prov.CreateInstance(ctx, provider.CreateSpec{
		ID:       vmid,
		Name:     name,
		Template: vm.Template,
		Node:     pveNode,
		Storage:  pveStorage,
		Arch:     "amd64",
		Cores:    vm.Cores,
		MemoryMB: vm.MemoryMB,
		DiskGB:   vm.DiskGB,
		Tags:     []string{pve.TagMayfly, modelServerTag},
		VLANTag:  vm.VLANTag,
		Firewall: vm.Firewall,
		HostPCI:  hostPCIDevices(vm.HostPCI),
		UserData: rendered.UserData,
		MetaData: rendered.MetaData,
	})
	if err != nil {
		return err
	}
	if err := inst.Start(ctx); err != nil {
		// The usual cause is the GPU: still bound to another running VM, or
		// its IOMMU group shared with a device the host needs.
		return fmt.Errorf("start %s (check the GPU is not passed to another running VM): %w", name, err)
	}

	e.logf("model-server: waiting for guest agent on %s", name)
	if err := inst.WaitForReady(ctx, 300); err != nil {
		return fmt.Errorf("guest agent: %w", err)
	}
	ip, _ := inst.PrimaryIPv4(ctx)

	e.St.ModelServer = &state.ModelServer{
		VMID:       vmid,
		Hostname:   name,
		PVENode:    pveNode,
		IP:         ip,
		Cores:      vm.Cores,
		MemoryMB:   vm.MemoryMB,
		DiskGB:     vm.DiskGB,
		Template:   vm.Template,
		VLANTag:    vm.VLANTag,
		HostPCI:    slices.Clone(vm.HostPCI),
		ConfigHash: ModelServerConfigHash(m),
		CreatedAt:  time.Now().UTC(),
	}
	if ip != "" {
		e.logf("model-server: OpenAI-compatible API will be at %s once the driver install and model pull finish (check: mayfly node exec model-server %s)",
			m.Endpoint(ip), modelserver.HealthPath)
	} else {
		e.logf("model-server: warning: no IPv4 from the guest agent yet — the next apply records the address")
	}
	return e.St.Save()
}

// updateModelServerVM pushes the setup files in place and applies resource
// changes. The driver and Ollama are not reinstalled.
func (e *Engine) updateModelServerVM(ctx context.Context, u *ModelServerUpdate) error {
	cur := e.St.ModelServer
	if cur == nil {
		return nil
	}
	prov, err := e.monitoringProvider()
	if err != nil {
		return err
	}
	inst, err := prov.GetInstance(ctx, cur.PVENode, cur.VMID)
	if err != nil {
		return fmt.Errorf("model-server vm %d: %w", cur.VMID, err)
	}
	vm := u.Server.VM.Resolved()

	if u.PushConfig {
		e.logf("model-server: pushing setup files + restarting ollama")
		if err := e.guestExec(ctx, inst, mkdirCmd(modelserver.Dirs()), 120); err != nil {
			return fmt.Errorf("create setup directories: %w", err)
		}
		for _, f := range modelserver.Files(modelServerSpec(u.Server)) {
			if err := inst.FileWrite(ctx, f.Path, f.Content); err != nil {
				return fmt.Errorf("write %s: %w", f.Path, err)
			}
		}
		if err := e.guestExec(ctx, inst, modelserver.ReconcileCommand(), 300); err != nil {
			return fmt.Errorf("reconcile model server: %w", err)
		}
		cur.ConfigHash = ModelServerConfigHash(u.Server)
	}

	if u.PowerCycle {
		e.logf("model-server: power cycling (cores %d→%d, mem %d→%dMB)",
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
		if err := inst.WaitForReady(ctx, 300); err != nil {
			return fmt.Errorf("guest agent after cycle: %w", err)
		}
		cur.Cores, cur.MemoryMB = vm.Cores, vm.MemoryMB
	}

	if u.DiskGrowGB > 0 {
		e.logf("model-server: growing disk %dG→%dG (live)", u.FromDiskGB, u.DiskGrowGB)
		if err := inst.ResizeDisk(ctx, u.DiskGrowGB); err != nil {
			return err
		}
		if err := e.guestExec(ctx, inst, growCmd("linux"), 120); err != nil {
			return fmt.Errorf("grow filesystem: %w", err)
		}
		cur.DiskGB = u.DiskGrowGB
	}
	// The endpoint is derived from the address, and a DHCP lease can move.
	if ip, err := inst.PrimaryIPv4(ctx); err == nil && ip != "" {
		cur.IP = ip
	}
	return e.St.Save()
}

// destroyModelServerVM removes the model-server VM, which releases its GPU.
// Pulled models go with the disk; they are re-pulled on the next create.
func (e *Engine) destroyModelServerVM(ctx context.Context) error {
	cur := e.St.ModelServer
	if cur == nil {
		return nil
	}
	e.logf("model-server: destroying vm (vmid %d)", cur.VMID)
	prov, err := e.monitoringProvider()
	if err != nil {
		return err
	}
	inst, err := prov.GetInstance(ctx, cur.PVENode, cur.VMID)
	if err != nil {
		e.St.ModelServer = nil
		return e.St.Save()
	}
	if running, _ := inst.IsRunning(ctx); running {
		if err := inst.Shutdown(ctx); err != nil {
			return err
		}
	}
	if err := inst.Delete(ctx); err != nil {
		return fmt.Errorf("delete model-server vm %d: %w", cur.VMID, err)
	}
	e.St.ModelServer = nil
	return e.St.Save()
}
