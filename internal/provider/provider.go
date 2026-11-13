// Package provider abstracts the hypervisor backend the engine drives to
// manage runner VMs. One backend implements it today: internal/pve (Proxmox
// VE). The abstraction is retained so the engine stays decoupled from the
// concrete backend.
//
// The interface deliberately mirrors the exact VM-lifecycle operations the
// engine performs — template sync, clone/create, in-guest config/power/disk
// work, and teardown — so that routing the engine through it is a
// behavior-preserving refactor of the previously Proxmox-coupled code. It does
// NOT cover the SSH bare-metal enrollment path (darwin / enrolled linux); that
// is not a hypervisor provider and stays in the engine.
package provider

import (
	"context"
	"errors"
)

// ErrInstanceNotFound is returned by GetInstance when the instance is gone
// (e.g. deleted outside mayfly). The engine treats it as "already destroyed".
var ErrInstanceNotFound = errors.New("instance not found")

// ErrTaskStillRunning means a backend task outlived mayfly's wait for it. The
// task has neither succeeded nor failed: the hypervisor is still working on
// it, so the caller must not record its result yet.
var ErrTaskStillRunning = errors.New("task still running when mayfly stopped waiting")

// ExecStatus is the outcome of an in-guest command execution.
type ExecStatus struct {
	ExitCode int
	OutData  string
	ErrData  string
}

// InstanceInfo is the subset of a live instance's state the engine's refresh
// pass compares against recorded state. A zero Cores/MemMB means the backend
// did not populate the field (don't invent drift from missing data).
type InstanceInfo struct {
	ID    int
	Cores int
	MemMB int
}

// TemplateSpec parameterizes EnsureTemplate. It matches the historical
// pve.TemplateSyncOptions shape so the Proxmox backend is a pass-through.
type TemplateSpec struct {
	Name     string // e.g. "mayfly-ubuntu-24.04-amd64"
	VMID     int    // fixed well-known template id (Proxmox)
	Arch     string // "amd64" | "arm64"
	Node     string // backend node to build on; empty = default
	Storage  string // storage/pool for image + disks; empty = configured default
	ImageURL string // cloud image URL
	Filename string // filename to store (basename of URL by default)
}

// CreateSpec describes a new instance cloned/launched from a template or image.
// It captures the whole create-time configuration the engine applied inline
// against Proxmox (cores/memory + optional disk grow + cloud-init + tags): the
// Proxmox backend clones then reconfigures, preserving the historical sequence.
type CreateSpec struct {
	ID       int    // allocated instance id (from NextID)
	Name     string // instance name, e.g. "mfl-<pool>-<id>"
	Template string // base template/image name
	Node     string // placement node; empty = default
	Storage  string // disk storage/pool; empty = default
	Arch     string // "amd64" | "arm64"

	// VMStorage, when set, places the VM's copied disk on a storage that can
	// differ from the template's. Empty (the default) does a LINKED clone,
	// which forces the disk onto the template's storage — byte-for-byte the
	// historical behavior. Non-empty does a FULL clone: the disk is COPIED onto
	// VMStorage, so it can live on a bigger/separate store than the
	// import-capable dir storage a template must sit on. Like CloudInitStorage,
	// it is a storage allowed to differ from the disk store — but purely
	// per-pool: there is no global default that would convert every pool to a
	// full clone. The tradeoff is real: a full clone copies the disk (slower
	// provision, consumes real space on VMStorage) where a linked clone only
	// references the template's.
	VMStorage string
	Cores     int      // vCPUs
	MemoryMB  int      // memory in MB
	DiskGB    int      // >0: resize the root disk to this size
	Tags      []string // instance tags/labels

	// VLANTag tags the instance's primary vNIC with an 802.1Q VLAN id
	// (1–4094); 0 = untagged, the historical behavior. Proxmox writes it as
	// net0 "tag=<n>" on the clone.
	VLANTag int
	// Firewall enables the hypervisor's per-vNIC firewall. Proxmox writes it
	// as net0 "firewall=1"; false = the historical behavior.
	Firewall bool

	// Cloud-init user-data + meta-data. Proxmox writes these to a generated
	// cloud-init ISO on CloudInitStorage.
	UserData         string
	MetaData         string
	CloudInitStorage string // Proxmox: storage the cloud-init ISO lands on; "" = disk storage

	// HostPCI passes host PCI devices (a GPU, typically) straight through to
	// the instance. Empty = none, the historical behavior. A passthrough
	// device can DMA into guest memory, so the hypervisor pins all of the
	// instance's RAM up front: it can never be ballooned or overcommitted.
	HostPCI []HostPCI
}

// HostPCI is one passed-through host PCI device. Exactly one of Host or
// Mapping is set.
type HostPCI struct {
	// Host is a raw PCI address, "0000:07:00.0" or "07:00.0" (Proxmox accepts
	// both; the short form means domain 0000, and leaving the function off
	// passes every function of the device).
	//
	// Proxmox only lets root@pam itself set a raw address — an API token,
	// even root@pam's own, is refused ("only root can set 'hostpci0' config
	// for non-mapped devices"). With token auth, use Mapping.
	Host string
	// Mapping names a Datacenter → Resource Mappings → PCI entry. Mappings
	// work with token auth and keep the host's bus address out of the config,
	// so a card can move slots without touching mayfly.
	Mapping string
	// PCIe presents the device on a PCIe bus, which needs the q35 machine
	// type; the provider switches the instance to q35 when any device sets
	// it. Off by default: compute cards like the Tesla P40 work on the plain
	// PCI bus of the default i440fx machine.
	PCIe bool
	// ROMBar exposes the device's option ROM to the guest. nil leaves the
	// hypervisor default (on).
	ROMBar *bool
	// XVGA makes the device the guest's primary display. Only for a GPU that
	// should drive the console; never for a headless compute card.
	XVGA bool
}

// Instance is an opaque handle to one live VM. Methods mirror the operations
// the engine performs on a member during provision, update, and teardown. The
// engine never inspects the concrete type.
type Instance interface {
	// ID returns the backend instance id (Proxmox VMID).
	ID() int

	// SetResources applies cores/memory. On Proxmox this is a config write that
	// takes effect on next boot (the engine power-cycles around it).
	SetResources(ctx context.Context, cores, memoryMB int) error

	// ResizeDisk grows the root disk to sizeGB.
	ResizeDisk(ctx context.Context, sizeGB int) error

	// MoveDisk moves the root disk onto storage while the instance runs,
	// deleting the source copy once the move completes. A move still running
	// when its wait budget ends returns an error wrapping ErrTaskStillRunning.
	MoveDisk(ctx context.Context, storage string) error

	// DiskStorage reports which storage the root disk lives on, and the
	// instance's config lock ("" when unlocked), read live from the backend.
	// It is how an interrupted move is recognised on the next apply.
	DiskStorage(ctx context.Context) (storage, lock string, err error)

	// DiskGB reports the root disk's currently provisioned size in GB, read
	// live from the backend. It is the source of truth for the resize
	// shrink-guard: recorded state can drift from reality after an out-of-band
	// resize, which is exactly the situation an operator resize command is used in.
	DiskGB(ctx context.Context) (int, error)

	// Start powers the instance on and returns once the request completes.
	Start(ctx context.Context) error

	// Shutdown does a graceful (ACPI) shutdown, falling back to a hard stop on
	// timeout. The backend owns the fallback so behavior matches the historical
	// Proxmox shutdown-then-stop dance.
	Shutdown(ctx context.Context) error

	// Stop powers the instance off immediately (a hard power-off), independent
	// of the guest OS and guest agent. Unlike Shutdown it sends no graceful ACPI
	// signal first — it is the recovery lever for a hung guest whose OS or guest
	// agent has stopped responding.
	Stop(ctx context.Context) error

	// IsRunning reports whether the instance is currently powered on.
	IsRunning(ctx context.Context) (bool, error)

	// WaitForReady blocks until the instance's in-guest exec channel is usable
	// (Proxmox: QEMU guest agent up), up to timeoutSec.
	WaitForReady(ctx context.Context, timeoutSec int) error

	// Exec runs argv in the guest and waits up to timeoutSec for it to exit.
	Exec(ctx context.Context, argv []string, timeoutSec int) (ExecStatus, error)

	// FileWrite writes content to path inside the guest.
	FileWrite(ctx context.Context, path string, content []byte) error

	// PrimaryIPv4 returns the guest's first usable LAN IPv4 (best-effort).
	PrimaryIPv4(ctx context.Context) (string, error)

	// Delete destroys the instance.
	Delete(ctx context.Context) error
}

// Provider is a hypervisor backend the engine drives to manage runner VMs.
type Provider interface {
	// EnsureTemplate idempotently creates the base template/image if absent.
	EnsureTemplate(ctx context.Context, spec TemplateSpec) error

	// TemplateExists reports whether a template named name exists on node
	// (empty = default node).
	TemplateExists(ctx context.Context, node, name string) (bool, error)

	// NextID allocates a fresh, unused instance id.
	NextID(ctx context.Context) (int, error)

	// CreateInstance clones/launches a new instance from spec.Template and
	// applies the create-time configuration (resources, disk, cloud-init,
	// tags). It does NOT start the instance — the engine starts it explicitly
	// so it can order tagging/start the same way across backends.
	CreateInstance(ctx context.Context, spec CreateSpec) (Instance, error)

	// GetInstance returns a handle to an existing instance by node + id.
	// Implementations return ErrInstanceNotFound when it is gone.
	GetInstance(ctx context.Context, node string, id int) (Instance, error)

	// ListInstances returns live instance info for every instance on node
	// (empty = default node), used by the engine's drift refresh.
	ListInstances(ctx context.Context, node string) ([]InstanceInfo, error)

	// VLANBridgeStatus resolves the host bridge a clone of template on node
	// would inherit (from the template's NIC) and reports whether that bridge is
	// VLAN-aware. It is the engine's per-pool vlan_tag preflight: a tag only
	// actually isolates the runner when its bridge is VLAN-aware, so the engine
	// fails loud when a tagged pool resolves to a bridge that is not. When the
	// template does not exist yet (a linux base the plan will auto-create on the
	// default bridge), it resolves the default bridge rather than erroring.
	//
	// If the host cannot be reached to perform the check, the returned error
	// wraps pve.ErrHostUnreachable so the engine can warn and skip that pool's
	// preflight (a down node can't be reconfigured) instead of aborting the
	// whole plan/apply. A reachable bridge that is simply not VLAN-aware is a
	// definite verdict (vlanAware=false, err=nil), not an error.
	VLANBridgeStatus(ctx context.Context, node, template string) (bridge string, vlanAware bool, err error)

	// NodeReachable reports whether node's hypervisor API can be reached right
	// now. It returns nil when reachable, or an error wrapping
	// pve.ErrHostUnreachable when the node's API cannot be reached at all (down,
	// connection refused, or a truncated body). apply() uses it to skip
	// maintenance ops — a template create, or a reconfig of an existing member —
	// that target a node which is simply down, warning and continuing instead of
	// aborting the whole reconcile. A node that is reachable returns nil even if
	// a later specific call against it fails, so genuine faults still surface at
	// the op that hits them rather than being masked here.
	NodeReachable(ctx context.Context, node string) error
}
