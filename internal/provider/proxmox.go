package provider

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/luthermonson/go-proxmox"

	"github.com/ephemerd/mayfly/internal/pve"
)

// Task-wait budgets, preserved verbatim from the engine's historical Proxmox
// timings so the refactor changes no behavior.
const (
	shortTask       = 5 * time.Minute
	longTask        = 30 * time.Minute
	shutdownTask    = 3 * time.Minute
	forceStopTask   = 2 * time.Minute
	agentExecPollHz = 2 * time.Second
)

// Proxmox adapts the existing pve.Client to the Provider interface. It holds
// the same client the engine used directly and reproduces the exact call
// sequences (clone → config → resize → cloud-init → tag, shutdown-then-stop
// fallback, guest-agent exec) so the Proxmox path is byte-for-byte the same.
type Proxmox struct {
	pv *pve.Client
}

// NewProxmox builds a Proxmox provider from an already-constructed pve.Client.
func NewProxmox(pv *pve.Client) *Proxmox {
	return &Proxmox{pv: pv}
}

func (p *Proxmox) EnsureTemplate(ctx context.Context, spec TemplateSpec) error {
	return p.pv.SyncTemplate(ctx, pve.TemplateSyncOptions{
		Name:     spec.Name,
		VMID:     spec.VMID,
		Arch:     spec.Arch,
		Node:     spec.Node,
		Storage:  spec.Storage,
		ImageURL: spec.ImageURL,
		Filename: spec.Filename,
	})
}

func (p *Proxmox) TemplateExists(ctx context.Context, node, name string) (bool, error) {
	if _, err := p.pv.FindTemplate(ctx, node, name); err != nil {
		return false, nil
	}
	return true, nil
}

func (p *Proxmox) NextID(ctx context.Context) (int, error) {
	return p.pv.NextID(ctx)
}

// cloneOptions builds the template-clone request for spec.
//
// With no VMStorage it is a LINKED clone (Full=false, no Storage) on the
// template's own storage — byte-for-byte the historical behavior, so an
// existing pool clones exactly as it did before this field existed.
//
// With VMStorage set it is a FULL clone whose copied disk lands on that
// storage. That is what lets the VM disk live on a bigger/separate store (e.g.
// a block thinpool) than the import-capable dir storage a template must sit on.
// A full clone COPIES the disk (slower to provision, consumes real space on
// VMStorage) where a linked clone only references the template's — the intended
// tradeoff. Lifted out of CreateInstance so the linked-vs-full decision is
// unit-testable without a live Proxmox.
func cloneOptions(spec CreateSpec) *proxmox.VirtualMachineCloneOptions {
	opts := &proxmox.VirtualMachineCloneOptions{
		NewID:  spec.ID,
		Name:   spec.Name,
		Target: spec.Node,
		Full:   proxmox.IntOrBool(false),
	}
	if spec.VMStorage != "" {
		opts.Full = proxmox.IntOrBool(true)
		opts.Storage = spec.VMStorage
	}
	return opts
}

// CreateInstance reproduces the engine's historical provision sequence up to
// (but not including) Start: clone the template, set cores/memory/agent/onboot,
// optionally grow the disk, write the cloud-init ISO, and add tags.
func (p *Proxmox) CreateInstance(ctx context.Context, spec CreateSpec) (Instance, error) {
	tmpl, err := p.pv.FindTemplate(ctx, spec.Node, spec.Template)
	if err != nil {
		return nil, err
	}
	_, cloneTask, err := tmpl.Clone(ctx, cloneOptions(spec))
	if err != nil {
		return nil, fmt.Errorf("clone: %w", err)
	}
	if err := cloneTask.Wait(ctx, 5*time.Second, longTask); err != nil {
		return nil, fmt.Errorf("wait for clone: %w", err)
	}

	node, err := p.pv.NodeByName(ctx, spec.Node)
	if err != nil {
		return nil, err
	}
	vm, err := node.VirtualMachine(ctx, spec.ID)
	if err != nil {
		return nil, err
	}

	opts := []proxmox.VirtualMachineOption{
		{Name: "cores", Value: spec.Cores},
		{Name: "memory", Value: spec.MemoryMB},
		{Name: "agent", Value: "1"},
		// Auto-start after a host reboot/power failure so the fleet self-heals.
		{Name: "onboot", Value: "1"},
	}
	base := ""
	if vm.VirtualMachineConfig != nil {
		base = vm.VirtualMachineConfig.Nets["net0"]
	}
	opts = append(opts, net0Options(base, spec.VLANTag, spec.Firewall)...)
	pciOpts, err := hostPCIOptions(spec.HostPCI)
	if err != nil {
		return nil, err
	}
	opts = append(opts, pciOpts...)

	cfgTask, err := vm.Config(ctx, opts...)
	if err != nil {
		return nil, err
	}
	if err := cfgTask.Wait(ctx, agentExecPollHz, shortTask); err != nil {
		return nil, err
	}

	if spec.DiskGB > 0 {
		resizeTask, err := vm.ResizeDisk(ctx, "scsi0", strconv.Itoa(spec.DiskGB)+"G")
		if err != nil {
			return nil, fmt.Errorf("resize disk: %w", err)
		}
		if err := resizeTask.Wait(ctx, agentExecPollHz, shortTask); err != nil {
			return nil, err
		}
	}

	// The cloud-init ISO can live on a different storage than the VM disk;
	// empty CloudInitStorage means keep it on the disk storage (historical).
	isoStorage := spec.CloudInitStorage
	if isoStorage == "" {
		isoStorage = spec.Storage
	}
	// arm64's `virt` machine has no IDE bus, so cloud-init must ride SCSI
	// there or QEMU aborts at start ("Bus 'ide.1' not found"). amd64 keeps
	// the historical ide2. Must match the slot the template build used.
	cloudinitDrive := "ide2"
	if spec.Arch == "arm64" {
		cloudinitDrive = "scsi1"
	}
	if err := vm.CloudInit(ctx, cloudinitDrive,
		spec.UserData, spec.MetaData, "", "",
		proxmox.WithCloudInitStorage(isoStorage),
	); err != nil {
		return nil, fmt.Errorf("cloud-init iso: %w", err)
	}

	for _, t := range spec.Tags {
		if _, err := vm.AddTag(ctx, proxmox.MakeTag(t)); err != nil && !proxmox.IsErrNoop(err) {
			return nil, fmt.Errorf("tag %s: %w", t, err)
		}
	}

	return &proxmoxInstance{vm: vm}, nil
}

// net0Options returns the net0 rewrite a clone needs for per-pool network
// isolation, or nothing at all when the pool asked for neither a VLAN tag nor
// the firewall.
//
// A linked clone inherits net0 from the template, so these settings cannot be
// applied at template-creation time — one template backs every pool of an
// arch, and the tag is per pool. They are written here, on the clone, before
// it is ever started. base is the clone's inherited net0 (model, generated
// MAC, bridge), which BuildNet0 preserves.
//
// Returning an empty slice when nothing is configured is deliberate: the
// config call then carries exactly the options it carried before this feature
// existed, so an unchanged config produces an unchanged VM.
func net0Options(base string, vlanTag int, firewall bool) []proxmox.VirtualMachineOption {
	if vlanTag <= 0 && !firewall {
		return nil
	}
	return []proxmox.VirtualMachineOption{
		{Name: "net0", Value: pve.BuildNet0(base, vlanTag, firewall)},
	}
}

// hostPCIOptions renders spec.HostPCI as Proxmox hostpciN options, plus the
// two settings passthrough forces on the VM:
//
//   - balloon=0. The device DMAs into guest RAM, so Proxmox pins all of it;
//     a balloon driver trying to hand pages back fights that pinning.
//   - machine=q35, but only when a device asked for PCIe. Switching an
//     i440fx template to q35 is a real hardware change for the guest, so it
//     is not done for devices that do not need it.
//
// go-proxmox has no typed hostpci option (VirtualMachineConfig carries the
// values back as plain strings), so these go through the same generic
// VirtualMachineOption setter net0 and the resources use. Lifted out of
// CreateInstance so the rendering is unit-testable without a live Proxmox.
func hostPCIOptions(devs []HostPCI) ([]proxmox.VirtualMachineOption, error) {
	if len(devs) == 0 {
		return nil, nil
	}
	// Proxmox numbers hostpci0..hostpci15.
	if len(devs) > 16 {
		return nil, fmt.Errorf("hostpci: %d devices, Proxmox allows at most 16", len(devs))
	}
	var opts []proxmox.VirtualMachineOption
	q35 := false
	for i, d := range devs {
		var parts []string
		switch {
		case d.Host != "" && d.Mapping != "":
			return nil, fmt.Errorf("hostpci%d: set host or mapping, not both", i)
		case d.Host != "":
			parts = append(parts, d.Host)
		case d.Mapping != "":
			parts = append(parts, "mapping="+d.Mapping)
		default:
			return nil, fmt.Errorf("hostpci%d: no host address or mapping", i)
		}
		if d.PCIe {
			parts = append(parts, "pcie=1")
			q35 = true
		}
		if d.ROMBar != nil {
			parts = append(parts, "rombar="+boolBit(*d.ROMBar))
		}
		if d.XVGA {
			parts = append(parts, "x-vga=1")
		}
		opts = append(opts, proxmox.VirtualMachineOption{
			Name:  fmt.Sprintf("hostpci%d", i),
			Value: strings.Join(parts, ","),
		})
	}
	opts = append(opts, proxmox.VirtualMachineOption{Name: "balloon", Value: 0})
	if q35 {
		opts = append(opts, proxmox.VirtualMachineOption{Name: "machine", Value: "q35"})
	}
	return opts, nil
}

func boolBit(b bool) string {
	if b {
		return "1"
	}
	return "0"
}

func (p *Proxmox) GetInstance(ctx context.Context, node string, id int) (Instance, error) {
	n, err := p.pv.NodeByName(ctx, node)
	if err != nil {
		return nil, err
	}
	vm, err := n.VirtualMachine(ctx, id)
	if err != nil {
		return nil, ErrInstanceNotFound
	}
	return &proxmoxInstance{vm: vm}, nil
}

func (p *Proxmox) ListInstances(ctx context.Context, node string) ([]InstanceInfo, error) {
	n, err := p.pv.NodeByName(ctx, node)
	if err != nil {
		return nil, err
	}
	vms, err := n.VirtualMachines(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]InstanceInfo, 0, len(vms))
	for _, vm := range vms {
		out = append(out, InstanceInfo{
			ID:    int(vm.VMID),
			Cores: vm.CPUs,
			MemMB: int(vm.MaxMem / (1 << 20)),
		})
	}
	return out, nil
}

// VLANBridgeStatus resolves the bridge a clone of template on node would
// inherit and reports whether it is VLAN-aware. The bridge comes from the
// template's own net0 (a linked clone inherits it verbatim; see BuildNet0), so
// the preflight checks the exact bridge the runner will use — not an assumed
// vmbr0. A missing template falls back to the default bridge (a linux base the
// plan auto-creates with DefaultNet0); any other template lookup path that
// errors also degrades to the default bridge so the vlan-awareness of the node
// still gets checked.
func (p *Proxmox) VLANBridgeStatus(ctx context.Context, node, template string) (string, bool, error) {
	bridge := pve.DefaultBridge
	if tmpl, err := p.pv.FindTemplate(ctx, node, template); err == nil && tmpl != nil && tmpl.VirtualMachineConfig != nil {
		bridge = pve.BridgeFromNet0(tmpl.VirtualMachineConfig.Nets["net0"])
	}
	aware, err := p.pv.BridgeVLANAware(ctx, node, bridge)
	if err != nil {
		return bridge, false, err
	}
	return bridge, aware, nil
}

// NodeReachable delegates to the pve client's connectivity probe, which wraps
// pve.ErrHostUnreachable when the node's API cannot be reached.
func (p *Proxmox) NodeReachable(ctx context.Context, node string) error {
	return p.pv.NodeReachable(ctx, node)
}

// proxmoxInstance wraps a live *proxmox.VirtualMachine as an Instance.
type proxmoxInstance struct {
	vm *proxmox.VirtualMachine
}

func (i *proxmoxInstance) ID() int { return int(i.vm.VMID) }

func (i *proxmoxInstance) SetResources(ctx context.Context, cores, memoryMB int) error {
	task, err := i.vm.Config(ctx,
		proxmox.VirtualMachineOption{Name: "cores", Value: cores},
		proxmox.VirtualMachineOption{Name: "memory", Value: memoryMB},
	)
	if err != nil {
		return err
	}
	return task.Wait(ctx, agentExecPollHz, shortTask)
}

func (i *proxmoxInstance) ResizeDisk(ctx context.Context, sizeGB int) error {
	task, err := i.vm.ResizeDisk(ctx, "scsi0", strconv.Itoa(sizeGB)+"G")
	if err != nil {
		return fmt.Errorf("resize disk: %w", err)
	}
	return checkTask(ctx, task, shortTask, "resize disk")
}

// MoveDisk is Proxmox's live disk move (qm disk move) of scsi0. delete=1 drops
// the source copy, so a linked clone becomes a standalone disk on storage. A
// big disk copies for a while, hence the long-task wait.
func (i *proxmoxInstance) MoveDisk(ctx context.Context, storage string) error {
	task, err := i.vm.MoveDisk(ctx, "scsi0", &proxmox.VirtualMachineMoveDiskOptions{
		Storage: storage,
		Delete:  true,
	})
	if err != nil {
		return fmt.Errorf("move disk to %s: %w", storage, err)
	}
	return checkTask(ctx, task, longTask, "move disk to "+storage)
}

// DiskStorage reports which storage scsi0 lives on (the part of its config
// string before ':') and the VM's config lock, if any, from the config
// GetInstance fetched.
func (i *proxmoxInstance) DiskStorage(_ context.Context) (storage, lock string, err error) {
	cfg := i.vm.VirtualMachineConfig
	if cfg == nil {
		return "", "", fmt.Errorf("vm %d: no config available to read the disk's storage", i.ID())
	}
	spec := cfg.SCSIs["scsi0"]
	storage, _, ok := strings.Cut(spec, ":")
	if !ok || storage == "" {
		return "", "", fmt.Errorf("vm %d: cannot read a storage from scsi0 %q", i.ID(), spec)
	}
	return storage, cfg.Lock, nil
}

// checkTask waits for a disk task and turns its outcome into an error.
//
// go-proxmox's Task.Wait returns nil as soon as a task stops running, whether
// it succeeded or FAILED, so waiting alone cannot tell a failed resize or move
// from a good one. This reads the exit status the way the power code does
// (taskOutcome). A task still running when the budget runs out is
// ErrTaskStillRunning: Proxmox keeps going, so the caller must not treat it
// as either success or failure.
func checkTask(ctx context.Context, task *proxmox.Task, max time.Duration, what string) error {
	switch why := taskOutcome(ctx, task, max); why {
	case "":
		return nil
	case taskTimedOut:
		return fmt.Errorf("%s: %w (task %s)", what, ErrTaskStillRunning, task.UPID)
	default:
		return fmt.Errorf("%s: %s (task %s)", what, why, task.UPID)
	}
}

// DiskGB reads the live provisioned size of the root disk (scsi0) from the VM's
// config. The instance was fetched fresh by GetInstance, so its config reflects
// current PVE — the read that lets the resize shrink-guard compare against
// reality rather than possibly-stale recorded state.
func (i *proxmoxInstance) DiskGB(_ context.Context) (int, error) {
	if i.vm.VirtualMachineConfig == nil {
		return 0, fmt.Errorf("vm %d: no config available to read disk size", i.ID())
	}
	spec := i.vm.VirtualMachineConfig.SCSIs["scsi0"]
	gb, err := diskSizeGB(spec)
	if err != nil {
		return 0, fmt.Errorf("vm %d: %w", i.ID(), err)
	}
	return gb, nil
}

// diskSizeGB extracts the provisioned size, in GB, from a Proxmox disk config
// string such as "local-lvm:vm-100-disk-0,size=60G". It errors when no size=
// field is present rather than guessing a size the guard could misuse.
func diskSizeGB(spec string) (int, error) {
	for _, field := range strings.Split(spec, ",") {
		v, ok := strings.CutPrefix(strings.TrimSpace(field), "size=")
		if !ok {
			continue
		}
		return parseSizeGB(v)
	}
	return 0, fmt.Errorf("no size= in disk config %q", spec)
}

// parseSizeGB converts a Proxmox size token (e.g. "60G", "1T", "512M", or a
// bare byte count) to whole GB, rounding down. Sub-GB values round to 0.
func parseSizeGB(s string) (int, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("empty disk size")
	}
	num := s
	mult := 1.0 / (1 << 30) // bare number is bytes
	switch s[len(s)-1] {
	case 'T', 't':
		num, mult = s[:len(s)-1], 1024
	case 'G', 'g':
		num, mult = s[:len(s)-1], 1
	case 'M', 'm':
		num, mult = s[:len(s)-1], 1.0/1024
	case 'K', 'k':
		num, mult = s[:len(s)-1], 1.0/(1<<20)
	}
	f, err := strconv.ParseFloat(strings.TrimSpace(num), 64)
	if err != nil {
		return 0, fmt.Errorf("parse disk size %q: %w", s, err)
	}
	return int(f * mult), nil
}

// Power transitions.
//
// A Proxmox power task finishing is NOT the power change happening, and the
// code here used to treat it as if it were. go-proxmox's Task.Wait returns nil
// as soon as a task stops running, whatever its exit status, so:
//
//   - a qmshutdown that FAILED ("VM quit/powerdown failed - got timeout", which
//     is what a guest with a dead guest agent produces) looked like a clean
//     shutdown, and the hard-stop fallback never ran;
//   - a qmstart that FAILED ("VM 100 already running", issued while that same
//     VM was still powering down) looked like a successful start.
//
// On 2026-10-10 that pair rebooted mfl-linux-amd64-node-b-100 into the STOPPED
// state: shutdown "succeeded", start "succeeded" against a VM that was still
// going down, the VM then finished stopping on its own, and the guest-agent
// wait failed with "VM 100 is not running". Someone had to power it on by hand.
//
// So every transition here is judged by the VM's own reported status, polled
// until it reaches the target state, never by the task alone.

// Power-state poll budgets. Vars so tests can compress them.
var (
	// powerPollInterval is the gap between status reads while waiting for a
	// transition.
	powerPollInterval = 2 * time.Second
	// shutdownFailedGrace is how long a VM whose shutdown task already failed
	// (or was refused) is given to finish going down anyway before mayfly
	// escalates to a hard stop. A failed graceful attempt may still have
	// reached the guest; a short wait avoids hard-stopping a VM that is a few
	// seconds from stopping cleanly.
	shutdownFailedGrace = 30 * time.Second
	// shutdownGraceBudget is the whole graceful-shutdown budget, measured from
	// the shutdown request: a VM still running at its end is hard-stopped.
	shutdownGraceBudget = shutdownTask
	// startSettle is how long a VM must be seen running after a start before
	// the start counts. It is what catches a VM that is still in the middle of
	// a shutdown: PVE reports it "running" right up until it stops.
	startSettle = 20 * time.Second
	// startAttempts bounds how many times Start re-issues qmstart for a VM
	// that keeps ending up stopped.
	startAttempts = 3
)

// Power states as PVE reports them in status/current.
const (
	powerRunning = "running"
	powerStopped = "stopped"
)

// powerState reads the VM's live status (status/current), refreshing the
// cached VirtualMachine.
func (i *proxmoxInstance) powerState(ctx context.Context) (string, error) {
	if err := i.vm.Ping(ctx); err != nil {
		return "", err
	}
	return i.vm.Status, nil
}

// waitForPower polls the VM's status until it reports want or within elapses.
// It returns whether want was reached and the last state seen.
func (i *proxmoxInstance) waitForPower(ctx context.Context, want string, within time.Duration) (bool, string) {
	deadline := time.Now().Add(within)
	last := ""
	for {
		if st, err := i.powerState(ctx); err == nil {
			last = st
			if st == want {
				return true, last
			}
		}
		if !time.Now().Before(deadline) {
			return false, last
		}
		select {
		case <-ctx.Done():
			return false, last
		case <-time.After(powerPollInterval):
		}
	}
}

// taskOutcome waits for a power task and describes how it ended: "" for a task
// that finished OK, otherwise why it did not (a failed exit status, a wait that
// timed out, an unreadable task).
func taskOutcome(ctx context.Context, task *proxmox.Task, max time.Duration) string {
	if err := task.Wait(ctx, agentExecPollHz, max); err != nil {
		if proxmox.IsTimeout(err) {
			return taskTimedOut
		}
		return err.Error()
	}
	if task.IsFailed || (task.ExitStatus != "" && task.ExitStatus != "OK") {
		if task.ExitStatus == "" {
			return "task failed"
		}
		return task.ExitStatus
	}
	return ""
}

// Start powers the VM on and returns once PVE reports it running and it has
// stayed running for startSettle. A start task that fails — notably "already
// running" against a VM that is really still powering down — is a cue to
// re-read the VM's state, not a success: if the VM then drops to stopped, the
// start is issued again.
func (i *proxmoxInstance) Start(ctx context.Context) error {
	var why string
	for attempt := 1; attempt <= startAttempts; attempt++ {
		task, err := i.vm.Start(ctx)
		switch {
		case err != nil:
			why = err.Error()
		default:
			why = taskOutcome(ctx, task, shortTask)
		}
		if why == "" {
			// The task says it started; confirm the VM agrees.
			if ok, _ := i.waitForPower(ctx, powerRunning, startSettle); !ok {
				why = "start task finished but the VM never reported running"
				continue
			}
		}
		// Whether the task succeeded or failed with "already running", the VM
		// must now stay running through the settle window. One that is mid-
		// shutdown drops to stopped inside it, and gets started again.
		if i.staysRunning(ctx, startSettle) {
			return nil
		}
		if why == "" {
			why = "VM stopped again right after starting"
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
	}
	return fmt.Errorf("vm %d did not come up after %d start attempts (last: %s)", i.ID(), startAttempts, why)
}

// staysRunning reports whether the VM is running now and is still running at
// the end of window, re-reading its status throughout. A VM seen stopped at any
// point returns false immediately.
func (i *proxmoxInstance) staysRunning(ctx context.Context, window time.Duration) bool {
	deadline := time.Now().Add(window)
	for {
		st, err := i.powerState(ctx)
		if err == nil && st == powerStopped {
			return false
		}
		if !time.Now().Before(deadline) {
			return err == nil && st == powerRunning
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(powerPollInterval):
		}
	}
}

// Shutdown does a graceful (ACPI / guest-agent) shutdown and returns only once
// PVE reports the VM stopped. When the graceful attempt fails, is refused, or
// does not finish within its budget, it escalates to a hard stop — and still
// waits for "stopped" rather than trusting the stop task.
func (i *proxmoxInstance) Shutdown(ctx context.Context) error {
	if st, err := i.powerState(ctx); err == nil && st == powerStopped {
		return nil
	}
	grace := shutdownFailedGrace
	var why string
	issued := time.Now()
	task, err := i.vm.Shutdown(ctx)
	if err != nil {
		why = err.Error()
	} else if why = taskOutcome(ctx, task, shutdownGraceBudget); why == "" {
		// A clean shutdown task should mean stopped already; allow the status
		// a moment to catch up.
		why = "shutdown task finished but the VM never reported stopped"
	} else if why == taskTimedOut {
		// The graceful budget is spent; do not wait on top of it.
		grace = 0
	} else if rest := shutdownGraceBudget - time.Since(issued); rest > grace {
		// The task failed, most often on PVE's own (shorter, ~60s) shutdown
		// timeout ("got timeout"). The ACPI / agent request still reached a
		// healthy guest, which may simply be slow to go down (a Windows node
		// applying updates). Give it the rest of mayfly's graceful budget
		// before hard-stopping it, rather than cutting it off 30s later.
		grace = rest
	}
	if ok, _ := i.waitForPower(ctx, powerStopped, grace); ok {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return i.hardStop(ctx, "graceful shutdown did not stop the VM ("+why+")")
}

// hardStop issues a hard power-off and waits for the VM to report stopped.
func (i *proxmoxInstance) hardStop(ctx context.Context, reason string) error {
	stopTask, err := i.vm.Stop(ctx)
	if err != nil {
		return fmt.Errorf("%s; hard stop refused: %w", reason, err)
	}
	outcome := taskOutcome(ctx, stopTask, forceStopTask)
	if ok, last := i.waitForPower(ctx, powerStopped, forceStopTask); !ok {
		if outcome == "" {
			outcome = "stop task finished"
		}
		return fmt.Errorf("%s; hard stop issued (%s) but vm %d still reports %q", reason, outcome, i.ID(), last)
	}
	return nil
}

// Stop is an immediate hard power-off (no graceful ACPI attempt), the recovery
// lever for a hung guest. A VM that is already stopped surfaces the backend's
// error to the caller. Like Shutdown, it returns only once the VM reports
// stopped.
func (i *proxmoxInstance) Stop(ctx context.Context) error {
	task, err := i.vm.Stop(ctx)
	if err != nil {
		return fmt.Errorf("stop: %w", err)
	}
	outcome := taskOutcome(ctx, task, forceStopTask)
	if ok, last := i.waitForPower(ctx, powerStopped, forceStopTask); !ok {
		if outcome == "" {
			outcome = "stop task finished"
		}
		return fmt.Errorf("stop: %s but vm %d still reports %q", outcome, i.ID(), last)
	}
	return nil
}

// IsRunning reads the VM's live status. The cached VirtualMachine is as old as
// the last API fetch, which for a handle obtained before a drain or power
// change is simply wrong.
func (i *proxmoxInstance) IsRunning(ctx context.Context) (bool, error) {
	st, err := i.powerState(ctx)
	if err != nil {
		return false, err
	}
	return st == powerRunning, nil
}

func (i *proxmoxInstance) WaitForReady(ctx context.Context, timeoutSec int) error {
	return i.vm.WaitForAgent(ctx, timeoutSec)
}

// Guest-agent exec polling.
//
// go-proxmox's WaitForAgentExecExit polls guest-exec-status every 100ms and
// aborts on the FIRST error it gets back. Both halves are wrong for the long
// commands mayfly runs in-guest:
//
//   - 100ms is ~10 qemu-guest-agent round trips per second for the entire
//     length of the command. During an ephemerd upgrade — a ~1 GB download
//     followed by a service stop/start — that is thousands of QMP round trips
//     against an agent whose host is busy, and PVE starts answering
//     "500 qga command 'guest-exec-status' failed - got timeout".
//   - aborting on that error reports the COMMAND as failed when all that
//     failed was mayfly's own polling. The guest process is untouched: QGA has
//     no kill, so it runs to completion regardless. On 2026-08-16 this turned
//     two Windows ephemerd upgrades that both fully succeeded into two
//     identical "got timeout" errors.
//
// So: poll gently, and treat a non-answering agent as a gap to ride out rather
// than a verdict. Only a gap that outlasts agentExecStatusGrace — long enough
// to cover a service restart, which is precisely when the agent goes quiet —
// is reported, and then as ErrExecStatusLost so callers can tell "I lost sight
// of this command" from "this command failed".
// The poll ramps rather than sitting at one interval, because the two things
// it has to be good at pull in opposite directions. A flat 100ms (the upstream
// default) hammers the agent for the whole length of a long command; a flat 2s
// taxes every short one, and an operator feels that on every invocation.
// Starting at 100ms and doubling to a 2s ceiling gets most of both: an upgrade
// settles into ~30 polls over 45s instead of ~450, while a short command is
// detected within a few hundred ms of finishing rather than up to 2s.
//
// Measured on this fleet, `mayfly node status` end to end: ~1.2s on the
// upstream flat 100ms, 3.9-5.4s on a flat 2s, 2.0-3.4s on this ramp. The
// residual over upstream is the ramp's early doubling; the ~2s floor is the
// command's own in-guest cost (dialing the control socket, one gRPC call) plus
// two Proxmox API round trips, and no polling interval touches that.
//
// Vars, not consts, so tests can compress them.
var (
	agentExecStatusPollMin = 100 * time.Millisecond
	agentExecStatusPollMax = 2 * time.Second
	agentExecStatusGrace   = 3 * time.Minute
)

// ErrExecStatusLost means the command was started but its outcome could not be
// observed: the guest agent stopped answering, or the caller's timeout expired
// while the command was still running. The command itself is still running (or
// has since finished) in the guest either way — nothing cancels it.
//
// Callers that can check the command's effect independently should do so
// instead of treating this as failure.
var ErrExecStatusLost = errors.New("lost track of the guest command; it is still running in the guest")

func (i *proxmoxInstance) Exec(ctx context.Context, argv []string, timeoutSec int) (ExecStatus, error) {
	pid, err := i.vm.AgentExec(ctx, argv, "")
	if err != nil {
		return ExecStatus{}, err
	}
	status, err := waitForAgentExec(ctx, func(ctx context.Context) (*proxmox.AgentExecStatus, error) {
		return i.vm.AgentExecStatus(ctx, pid)
	}, pid, time.Duration(timeoutSec)*time.Second)
	if err != nil {
		return ExecStatus{}, err
	}
	return ExecStatus{
		ExitCode: status.ExitCode,
		OutData:  status.OutData,
		ErrData:  status.ErrData,
	}, nil
}

// agentExecStatusFn fetches one guest-exec-status sample. Lifted out of the
// VirtualMachine so the tolerance logic is testable without a live hypervisor.
type agentExecStatusFn func(context.Context) (*proxmox.AgentExecStatus, error)

// waitForAgentExec polls guest-exec-status for pid until the command exits,
// tolerating a guest agent that stops answering for up to agentExecStatusGrace.
func waitForAgentExec(ctx context.Context, poll agentExecStatusFn, pid int, timeout time.Duration) (*proxmox.AgentExecStatus, error) {
	deadline := time.Now().Add(timeout)
	wait := agentExecStatusPollMin

	var lastErr error
	var lostSince time.Time
	for {
		status, err := poll(ctx)
		switch {
		// A nil status with a nil error means PVE answered with no result
		// body. Nothing to read, so treat it exactly like an unreachable
		// agent — ride it out — rather than dereferencing it.
		case err == nil && status != nil:
			lastErr, lostSince = nil, time.Time{}
			if status.Exited != 0 {
				return status, nil
			}
		case ctx.Err() != nil:
			return nil, ctx.Err()
		default:
			if err == nil {
				err = errors.New("guest agent returned an empty exec-status")
			}
			lastErr = err
			if lostSince.IsZero() {
				lostSince = time.Now()
			}
			if time.Since(lostSince) > agentExecStatusGrace {
				return nil, fmt.Errorf("%w: guest agent stopped answering for %s (pid %d): %w",
					ErrExecStatusLost, agentExecStatusGrace, pid, lastErr)
			}
		}

		if timeout > 0 && time.Now().After(deadline) {
			if lastErr != nil {
				return nil, fmt.Errorf("%w: still running after %s (pid %d), agent last said: %w",
					ErrExecStatusLost, timeout, pid, lastErr)
			}
			return nil, fmt.Errorf("%w: still running after %s (pid %d)", ErrExecStatusLost, timeout, pid)
		}

		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(wait):
		}
		if wait < agentExecStatusPollMax {
			if wait *= 2; wait > agentExecStatusPollMax {
				wait = agentExecStatusPollMax
			}
		}
	}
}

func (i *proxmoxInstance) FileWrite(ctx context.Context, path string, content []byte) error {
	return i.vm.AgentFileWrite(ctx, path, content)
}

func (i *proxmoxInstance) PrimaryIPv4(ctx context.Context) (string, error) {
	return pve.PrimaryIPv4(ctx, i.vm)
}

func (i *proxmoxInstance) Delete(ctx context.Context) error {
	// Proxmox refuses to destroy a running VM ("VM <id> is running - destroy
	// failed"), which aborts a rolling replace after the fresh member is
	// already up and leaves the old one running. Power it off first. The runner
	// is disposable, so ensureStopped's graceful-ACPI-then-hard-stop is fine.
	if err := i.ensureStopped(ctx); err != nil {
		return err
	}
	task, err := i.vm.Delete(ctx, nil)
	if err != nil {
		return fmt.Errorf("delete vm %d: %w", i.ID(), err)
	}
	return task.Wait(ctx, agentExecPollHz, shortTask)
}

// ensureStopped powers the instance off if it is running so a following Delete
// is accepted by Proxmox. It refreshes the cached power state first: IsRunning
// reads the status as of the last API fetch, which — for a member handed over
// minutes earlier and drained since — can be stale. A VM that is already
// stopped is left untouched (Shutdown would error "not running"); a VM that has
// vanished is treated as already stopped so Delete surfaces the not-found.
func (i *proxmoxInstance) ensureStopped(ctx context.Context) error {
	if err := i.vm.Ping(ctx); err != nil {
		// Status unreadable — most likely already deleted. Let Delete decide.
		return nil
	}
	if i.vm.IsStopped() {
		return nil
	}
	return i.Shutdown(ctx)
}

// taskTimedOut is taskOutcome's answer for a task still running when its wait
// budget ran out.
const taskTimedOut = "task still running when its wait budget ran out"
