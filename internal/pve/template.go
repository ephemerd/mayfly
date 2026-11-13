package pve

import (
	"context"
	"fmt"
	"path"
	"strings"
	"time"

	"github.com/luthermonson/go-proxmox"
)

// TemplateSyncOptions parameterizes SyncTemplate.
type TemplateSyncOptions struct {
	Name     string // e.g. "mayfly-ubuntu-24.04-amd64"
	VMID     int    // e.g. 9000 (amd64) / 9001 (arm64)
	Arch     string // "amd64" | "arm64"
	Node     string // PVE node to build on; empty = default node
	Storage  string // storage for image + disks; empty = configured default
	ImageURL string // cloud image URL
	Filename string // filename to store on Proxmox (basename of URL by default)
}

const (
	taskShort = 5 * time.Minute
	taskLong  = 30 * time.Minute
)

// DefaultBridge is the host bridge every mayfly template is created on, and the
// fallback a VLAN preflight targets when a template's net0 names no bridge.
const DefaultBridge = "vmbr0"

// DefaultNet0 is the network device every mayfly template is created with:
// a virtio NIC on the default bridge, untagged, no firewall. Clones inherit
// it, so per-pool network settings are applied to the clone, not here.
const DefaultNet0 = "virtio,bridge=" + DefaultBridge

// BridgeFromNet0 extracts the host bridge a clone would land on from a Proxmox
// net0 value (e.g. "virtio=BC:24:11:00:00:01,bridge=vmbr0,tag=42" → "vmbr0").
// A linked clone inherits its NIC — bridge included — from the template's net0,
// so this is the exact bridge a VLAN preflight must check for vlan-awareness.
// An empty net0, or one carrying no bridge= clause, falls back to DefaultBridge.
func BridgeFromNet0(net0 string) string {
	for _, f := range strings.Split(net0, ",") {
		if v, ok := strings.CutPrefix(f, "bridge="); ok && v != "" {
			return v
		}
	}
	return DefaultBridge
}

// BuildNet0 returns the Proxmox net0 value for a runner NIC, layering an
// optional VLAN tag and the per-NIC firewall flag on top of base — the value
// the VM already has (inherited from the template at clone time, e.g.
// "virtio=BC:24:11:00:00:01,bridge=vmbr0"). base is preserved verbatim apart
// from any tag=/firewall= clauses, so the model, generated MAC, and bridge all
// survive; an empty base falls back to DefaultNet0.
//
// vlanTag <= 0 omits "tag="; firewall false omits "firewall=". When neither is
// requested the caller should skip the config write entirely (see
// Proxmox.CreateInstance) so an unconfigured pool produces byte-identical VM
// config to before this knob existed.
//
// NOTE: "firewall=1" on the NIC is necessary but NOT sufficient. The Proxmox
// firewall must also be enabled at the datacenter level (Datacenter →
// Firewall → Options → Firewall: Yes) and at the VM level (VM → Firewall →
// Options), and the default input/output policy plus the actual rules are
// configured outside mayfly. Enabling this flag alone changes nothing;
// enabling it together with a datacenter/VM firewall whose input policy is
// DROP will cut the runner off until rules are in place.
func BuildNet0(base string, vlanTag int, firewall bool) string {
	if base == "" {
		base = DefaultNet0
	}
	parts := make([]string, 0, len(strings.Split(base, ","))+2)
	for _, f := range strings.Split(base, ",") {
		if f == "" {
			continue
		}
		// Drop any tag/firewall the VM already carries; this function is the
		// single source of truth for both.
		if strings.HasPrefix(f, "tag=") || strings.HasPrefix(f, "firewall=") {
			continue
		}
		parts = append(parts, f)
	}
	if vlanTag > 0 {
		parts = append(parts, fmt.Sprintf("tag=%d", vlanTag))
	}
	if firewall {
		parts = append(parts, "firewall=1")
	}
	return strings.Join(parts, ",")
}

// SyncTemplate is idempotent: if a template named opts.Name already exists on
// the target node, it returns without doing anything. Otherwise it downloads
// the cloud image into storage (import content type), creates a VM at
// opts.VMID with the disk imported from that image, wires up cloud-init +
// serial console, and marks the VM as a template.
//
// arm64 cloud images are UEFI-only, so arm64 templates get bios=ovmf plus an
// EFI vars disk; amd64 keeps SeaBIOS.
func (c *Client) SyncTemplate(ctx context.Context, opts TemplateSyncOptions) error {
	if opts.Name == "" || opts.VMID == 0 || opts.ImageURL == "" {
		return fmt.Errorf("template sync: name, vmid, and image_url are required")
	}
	if opts.Arch == "" {
		opts.Arch = "amd64"
	}
	if opts.Storage == "" {
		opts.Storage = c.cfg.Storage
	}
	if opts.Filename == "" {
		opts.Filename = path.Base(opts.ImageURL)
	}
	// Ubuntu cloud images are qcow2 despite the .img name, and Proxmox's
	// "import" content type rejects .img ("invalid filename or wrong
	// extension") — store as .qcow2 so download-url and import-from accept it.
	if strings.HasSuffix(opts.Filename, ".img") {
		opts.Filename = strings.TrimSuffix(opts.Filename, ".img") + ".qcow2"
	}

	if existing, err := c.FindTemplate(ctx, opts.Node, opts.Name); err == nil && existing != nil {
		return nil
	}

	node, err := c.NodeByName(ctx, opts.Node)
	if err != nil {
		return err
	}

	storage, err := node.Storage(ctx, opts.Storage)
	if err != nil {
		return fmt.Errorf("get storage %s: %w", opts.Storage, err)
	}
	if !strings.Contains(storage.Content, "import") {
		return fmt.Errorf("storage %s does not accept 'import' content (has %q); pick a storage that does",
			storage.Name, storage.Content)
	}

	if err := ensureImageDownloaded(ctx, storage, opts.Filename, opts.ImageURL); err != nil {
		return err
	}

	createOpts := []proxmox.VirtualMachineOption{
		{Name: "name", Value: opts.Name},
		{Name: "memory", Value: 2048},
		{Name: "cores", Value: 2},
		{Name: "cpu", Value: "host"},
		{Name: "net0", Value: DefaultNet0},
		{Name: "scsihw", Value: "virtio-scsi-pci"},
		{Name: "agent", Value: "1"},
		{Name: "serial0", Value: "socket"},
		{Name: "vga", Value: "serial0"},
		{Name: "ostype", Value: "l26"},
	}
	if opts.Arch == "arm64" {
		createOpts = append(createOpts,
			proxmox.VirtualMachineOption{Name: "bios", Value: "ovmf"},
			proxmox.VirtualMachineOption{Name: "efidisk0", Value: fmt.Sprintf("%s:1,efitype=4m,pre-enrolled-keys=0", opts.Storage)},
		)
	}

	// The well-known template VMID (9000 amd64 / 9001 arm64) is only a
	// convention — templates are resolved by NAME everywhere else. VMIDs are
	// cluster-wide unique, so a SECOND same-arch node in the cluster collides
	// on the fixed id (e.g. node-a already holds 9000 when node-b builds its
	// own amd64 template). Fall back to any free id in that case.
	vmid := opts.VMID
	createTask, err := node.NewVirtualMachine(ctx, vmid, createOpts...)
	if err != nil && strings.Contains(err.Error(), "already exists") {
		free, nerr := c.NextID(ctx)
		if nerr != nil {
			return fmt.Errorf("template vmid %d taken cluster-wide and NextID failed: %w", vmid, nerr)
		}
		vmid = free
		createTask, err = node.NewVirtualMachine(ctx, vmid, createOpts...)
	}
	if err != nil {
		return fmt.Errorf("create template vm: %w", err)
	}
	if err := createTask.Wait(ctx, 2*time.Second, taskShort); err != nil {
		return fmt.Errorf("wait for template vm creation: %w", err)
	}

	vm, err := node.VirtualMachine(ctx, vmid)
	if err != nil {
		return err
	}

	importSpec := fmt.Sprintf("%s:0,import-from=%s:import/%s,discard=on,ssd=1",
		opts.Storage, opts.Storage, opts.Filename)
	diskTask, err := vm.Config(ctx,
		proxmox.VirtualMachineOption{Name: "scsi0", Value: importSpec},
	)
	if err != nil {
		return fmt.Errorf("attach imported disk: %w", err)
	}
	if err := diskTask.Wait(ctx, 5*time.Second, taskLong); err != nil {
		return fmt.Errorf("wait for disk import: %w", err)
	}

	// The cloud-init drive must sit on a bus the guest machine actually has.
	// The arm64 `virt` machine has NO IDE controller, so an ide2 cloud-init
	// drive makes QEMU abort at start ("Bus 'ide.1' not found"). Put it on the
	// SCSI controller for arm64; amd64 keeps the historical ide2.
	cloudinitDrive := "ide2"
	if opts.Arch == "arm64" {
		cloudinitDrive = "scsi1"
	}
	finalTask, err := vm.Config(ctx,
		proxmox.VirtualMachineOption{Name: cloudinitDrive, Value: fmt.Sprintf("%s:cloudinit", opts.Storage)},
		proxmox.VirtualMachineOption{Name: "boot", Value: "order=scsi0"},
	)
	if err != nil {
		return fmt.Errorf("configure cloudinit + boot: %w", err)
	}
	if err := finalTask.Wait(ctx, 2*time.Second, taskShort); err != nil {
		return fmt.Errorf("wait for final config: %w", err)
	}

	tmplTask, err := vm.ConvertToTemplate(ctx)
	if err != nil {
		return fmt.Errorf("convert to template: %w", err)
	}
	if err := tmplTask.Wait(ctx, 2*time.Second, taskShort); err != nil {
		return fmt.Errorf("wait for template conversion: %w", err)
	}

	return nil
}

func ensureImageDownloaded(ctx context.Context, storage *proxmox.Storage, filename, imageURL string) error {
	content, err := storage.GetContent(ctx)
	if err != nil {
		return fmt.Errorf("list storage content: %w", err)
	}
	for _, c := range content {
		if strings.HasSuffix(c.Volid, "/"+filename) {
			return nil
		}
	}
	task, err := storage.DownloadURL(ctx, "import", filename, imageURL)
	if err != nil {
		return fmt.Errorf("download %s: %w", imageURL, err)
	}
	if err := task.Wait(ctx, 5*time.Second, taskLong); err != nil {
		return fmt.Errorf("wait for image download: %w", err)
	}
	return nil
}
