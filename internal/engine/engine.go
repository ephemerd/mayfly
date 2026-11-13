package engine

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"sort"

	"github.com/ephemerd/mayfly/internal/cloudflared"
	"github.com/ephemerd/mayfly/internal/config"
	"github.com/ephemerd/mayfly/internal/ghapp"
	"github.com/ephemerd/mayfly/internal/monitoring"
	"github.com/ephemerd/mayfly/internal/provider"
	"github.com/ephemerd/mayfly/internal/pve"
	"github.com/ephemerd/mayfly/internal/state"
)

type Engine struct {
	Cfg *config.Config
	St  *state.Store
	// provCache holds the hypervisor backend built on demand (Proxmox, shared
	// by every VM pool). Populated lazily via providerFor / providerByName so a
	// fleet only pays to construct the backend when it actually provisions VMs.
	// The SSH bare-metal enrollment path never touches this.
	provCache map[config.Provider]provider.Provider
	CF        cloudflared.Provider
	GH        *ghapp.Credentials
	Out       io.Writer
}

func New(cfg *config.Config, st *state.Store, out io.Writer) (*Engine, error) {
	gh, err := ghapp.Load(cfg.GitHub)
	if err != nil {
		return nil, err
	}
	cf, err := cloudflared.New(cfg.Cloudflare)
	if err != nil {
		return nil, err
	}
	return &Engine{
		Cfg:       cfg,
		St:        st,
		provCache: map[config.Provider]provider.Provider{},
		CF:        cf,
		GH:        gh,
		Out:       out,
	}, nil
}

// providerFor returns the cached hypervisor backend for the given pool spec,
// keyed off its ResolvedProvider (proxmox by default). It lazily constructs
// and caches one backend per provider so all pools sharing a backend reuse the
// same client. Enrolled (SSH bare-metal) pools have no hypervisor and must not
// call this.
func (e *Engine) providerFor(spec config.PoolSpec) (provider.Provider, error) {
	return e.providerByName(spec.ResolvedProvider())
}

// providerByName returns the cached backend for a provider name, building it on
// first use. Used by state-driven paths (destroy, refresh) where the spec may
// be unavailable — resolve the name via providerForPool then call this.
func (e *Engine) providerByName(name config.Provider) (provider.Provider, error) {
	if p, ok := e.provCache[name]; ok {
		return p, nil
	}
	p, err := newProvider(e.Cfg, name)
	if err != nil {
		return nil, err
	}
	e.provCache[name] = p
	return p, nil
}

// newProvider constructs a hypervisor backend by name, uncached. Shared by the
// engine's per-pool cache above and by the node-operation executors
// (NewNodeExecutor), so an operator command reaches a backend exactly the way
// apply does. Proxmox is the only backend; the name is retained for the cache
// key and forward compatibility.
func newProvider(cfg *config.Config, _ config.Provider) (provider.Provider, error) {
	return provider.NewProxmox(pve.New(cfg.Proxmox)), nil
}

// providerForPool resolves the backend for a pool identified by name, looking
// up its provider in the live config. Pools no longer in config (e.g. mid
// destroy of a removed pool) default to Proxmox — the historical backend and a
// safe fallback for VM teardown. Returns Proxmox for enrolled pools too, but
// those paths never reach a hypervisor call.
func (e *Engine) providerForPool(name string) (provider.Provider, error) {
	if spec, ok := e.Cfg.Pools[name]; ok {
		return e.providerFor(spec)
	}
	return e.providerByName(config.ProviderProxmox)
}

// monitoringProvider returns the backend the monitoring node runs on. The
// monitoring VM is an x86 host on the Proxmox nodes (node-a/node-c), so it
// always uses the Proxmox backend regardless of any pool's provider.
func (e *Engine) monitoringProvider() (provider.Provider, error) {
	return e.providerByName(config.ProviderProxmox)
}

// BuildPlan optionally refreshes state against live infra, checks live PVE
// for every template the config references, and diffs config against state.
// Missing linux templates become plan items; a missing windows template is a
// hard error (they can't be auto-created). forceReplace optionally names a
// pool whose members get rebuilt regardless of drift tier — the escape hatch
// for wedged nodes.
func (e *Engine) BuildPlan(ctx context.Context, forceReplace string, refresh, sshProbe bool) (*Plan, *RefreshResult, error) {
	rr := &RefreshResult{}
	if refresh {
		var err error
		if rr, err = e.Refresh(ctx, sshProbe); err != nil {
			return nil, nil, err
		}
	}

	seen := map[string]bool{} // provider/template@node → checked
	var missing []TemplateCreate
	for name, raw := range e.Cfg.Pools {
		spec := raw.Resolved(e.Cfg.Defaults)
		if spec.UsesEnroll() {
			continue // bare metal (darwin or enrolled linux): no template, no PVE
		}
		prov, err := e.providerFor(spec)
		if err != nil {
			return nil, nil, fmt.Errorf("pool %s: %w", name, err)
		}
		node, storage := spec.ResolvedPlacement(e.Cfg.Proxmox)
		key := string(spec.ResolvedProvider()) + "/" + spec.Template + "@" + node
		if seen[key] {
			continue
		}
		seen[key] = true
		if ok, err := prov.TemplateExists(ctx, node, spec.Template); err == nil && ok {
			continue
		}
		if spec.OS == "windows" {
			return nil, nil, fmt.Errorf(
				"pool %s: windows template %q not found on node %s — build it once (Cloudbase-Init + QEMU guest agent + sysprep) and convert to template",
				name, spec.Template, node)
		}
		missing = append(missing, TemplateCreate{
			Name: spec.Template, Arch: spec.Arch, Node: node, Storage: storage,
			Provider: spec.ResolvedProvider(),
		})
	}
	// The monitoring node is a linux VM on Proxmox — auto-create its base
	// template if the fleet doesn't already reference it.
	if e.Cfg.Monitoring != nil {
		mprov, err := e.monitoringProvider()
		if err != nil {
			return nil, nil, fmt.Errorf("monitoring: %w", err)
		}
		spec := e.Cfg.Monitoring.Resolved()
		node, storage := spec.ResolvedPlacement(e.Cfg.Proxmox)
		key := string(config.ProviderProxmox) + "/" + spec.Template + "@" + node
		if !seen[key] {
			seen[key] = true
			if ok, err := mprov.TemplateExists(ctx, node, spec.Template); err != nil || !ok {
				missing = append(missing, TemplateCreate{
					Name: spec.Template, Arch: "amd64", Node: node, Storage: storage,
					Provider: config.ProviderProxmox,
				})
			}
		}
	}
	sort.Slice(missing, func(i, j int) bool { return missing[i].Name < missing[j].Name })

	// Fail loud, before anything is provisioned, if a tagged pool would land on
	// a bridge that can't carry the tag — the feature's core fail-open.
	if err := e.preflightVLAN(ctx); err != nil {
		return nil, nil, err
	}

	// Rewrite still-valid pre-"c2:" config fingerprints in the current format
	// (in memory; apply persists it), so the next version bump is judged on
	// the binary alone. Diff copes with legacy hashes either way.
	migrateLegacyHashes(e.Cfg, e.St, e.GH.PrivateKeyPEM)

	return Diff(e.Cfg, e.St, e.GH.PrivateKeyPEM, missing, forceReplace), rr, nil
}

// preflightVLAN refuses the plan when any VM pool requests a vlan_tag but the
// host bridge its clones would inherit is not VLAN-aware. On a plain bridge
// Proxmox writes net0 "tag=<n>" and boots the VM, yet the guest never reaches
// the VLAN — the exact silent isolation failure this feature is prone to. The
// check runs against live PVE here, so both `mayfly plan` and `mayfly apply`
// refuse rather than provision a runner that only thinks it is isolated.
//
// Verdicts are cached per (node, template) since pools sharing a template share
// a bridge; pools are visited in name order so the surfaced error is stable.
// Enrolled (bare-metal) pools have no clone/bridge and are skipped.
func (e *Engine) preflightVLAN(ctx context.Context) error {
	type key struct{ node, template string }
	type verdict struct {
		bridge string
		aware  bool
	}
	checked := map[key]verdict{}

	names := make([]string, 0, len(e.Cfg.Pools))
	for name := range e.Cfg.Pools {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, name := range names {
		spec := e.Cfg.Pools[name].Resolved(e.Cfg.Defaults)
		if spec.VLANTag <= 0 || spec.UsesEnroll() {
			continue
		}
		prov, err := e.providerFor(spec)
		if err != nil {
			return fmt.Errorf("pool %s: %w", name, err)
		}
		node, _ := spec.ResolvedPlacement(e.Cfg.Proxmox)
		k := key{node, spec.Template}
		v, ok := checked[k]
		if !ok {
			bridge, aware, err := prov.VLANBridgeStatus(ctx, node, spec.Template)
			if err != nil {
				// Fail soft when the host is simply unreachable: a node we can't
				// reach can't be reconfigured anyway, and one down node must not
				// abort reconciliation for the whole fleet. Warn and skip this
				// pool's VLAN check. A reachable-but-misconfigured bridge returns
				// a definite verdict (aware=false, no error) and is still caught
				// by vlanBridgeError below, so genuine fail-open stays a hard error.
				if errors.Is(err, pve.ErrHostUnreachable) {
					e.logf("warning: pool %s: node %s is unreachable, skipping vlan_tag=%d bridge preflight (%v); "+
						"cannot verify the bridge is VLAN-aware — rerun once the node is back", name, node, spec.VLANTag, err)
					continue
				}
				return fmt.Errorf("pool %s: cannot verify bridge %s on node %s is VLAN-aware for vlan_tag=%d: %w",
					name, bridge, node, spec.VLANTag, err)
			}
			v = verdict{bridge: bridge, aware: aware}
			checked[k] = v
		}
		if err := vlanBridgeError(name, spec.VLANTag, node, v.bridge, v.aware); err != nil {
			return err
		}
	}
	return nil
}

// DestroyPlan builds a plan that removes the named pool, or every pool when
// name is empty. The template is left alone.
func (e *Engine) DestroyPlan(name string) (*Plan, error) {
	p := &Plan{}
	if name == "" {
		for _, pool := range e.St.Pools {
			p.PoolDestroys = append(p.PoolDestroys, pool.Name)
		}
		p.MonitoringDestroy = e.St.Monitoring != nil
		p.RegistryCacheDestroy = e.St.RegistryCache != nil
		p.ModelServerDestroy = e.St.ModelServer != nil
		return p, nil
	}
	if _, ok := e.St.FindPool(name); !ok {
		return nil, fmt.Errorf("no pool named %q in state", name)
	}
	p.PoolDestroys = []string{name}
	return p, nil
}

func (e *Engine) logf(format string, args ...any) {
	fmt.Fprintf(e.Out, format+"\n", args...)
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// Apply executes the plan in dependency order: template first, then pool
// creates, replacements, scale-ups, scale-downs, and pool destroys last.
// State is saved after every mutating step so a failed apply is resumable
// by re-running apply.
//
// It always ends by saying how it ended: a summary line on success, an error
// (which the CLI turns into a non-zero exit) otherwise. The success path used
// to print nothing after a member's last progress line, so a completed apply
// was indistinguishable from one that had died mid-provision.
func (e *Engine) Apply(ctx context.Context, p *Plan) error {
	adds, changes, destroys := p.Counts()
	if err := e.apply(ctx, p); err != nil {
		return err
	}
	e.logf("\nApply complete: %d added, %d changed, %d destroyed.", adds, changes, destroys)
	return nil
}

// foldTargetDrift adds a scrape-target refresh to the plan when this apply's
// own member changes moved the target set. The plan's monitoring diff was
// computed against PRE-apply state (Diff cannot know the IP a member that does
// not exist yet will get), so without this a scale-up or replace left the
// monitoring node scraping the old set until a second apply.
func (e *Engine) foldTargetDrift(p *Plan) {
	if e.Cfg.Monitoring == nil || e.St.Monitoring == nil || p.MonitoringCreate != nil || p.MonitoringDestroy {
		return
	}
	spec := e.Cfg.Monitoring.Resolved()
	if monitoring.TargetsHash(e.St, spec.MetricsPort) == e.St.Monitoring.TargetHash {
		return
	}
	if p.MonitoringUpdate == nil {
		cur := e.St.Monitoring
		p.MonitoringUpdate = &MonitoringUpdate{
			Spec: spec, Cache: e.Cfg.RegistryCache.OnMonitoring(), VMID: cur.VMID,
			FromCores: cur.Cores, FromMemMB: cur.MemoryMB, FromDiskGB: cur.DiskGB,
		}
	}
	if !p.MonitoringUpdate.PushTargets && !p.MonitoringUpdate.PushConfig {
		e.logf("monitoring: member set changed during this apply; refreshing scrape targets")
	}
	p.MonitoringUpdate.PushTargets = true
}

// reachCache memoizes node reachability across a single apply so a dead node is
// probed once, not once per op that targets it.
type reachCache map[string]bool

// nodeDown reports whether node's hypervisor API is unreachable right now,
// memoizing the verdict for this apply. Only a connectivity failure
// (pve.ErrHostUnreachable) counts as down; any other probe error is treated as
// reachable so the real fault still surfaces at the op that hits it. An empty
// node means the provider's default node.
func (e *Engine) nodeDown(ctx context.Context, prov provider.Provider, node string, cache reachCache) bool {
	key := node
	if key == "" {
		key = e.Cfg.Proxmox.Node
	}
	if v, ok := cache[key]; ok {
		return v
	}
	down := errors.Is(prov.NodeReachable(ctx, node), pve.ErrHostUnreachable)
	cache[key] = down
	return down
}

// updateTarget resolves the hypervisor node and provider a pool update acts on,
// so apply can skip it when that node is down. ok is false for an SSH-enrolled
// (darwin) pool — there is no hypervisor node to probe — or when the provider
// cannot be resolved; the caller then runs the update unguarded, preserving the
// prior always-run behaviour.
func (e *Engine) updateTarget(u UpdateChange) (node string, prov provider.Provider, ok bool) {
	if u.Spec.UsesEnroll() {
		return "", nil, false
	}
	prov, err := e.providerFor(u.Spec)
	if err != nil {
		return "", nil, false
	}
	node, _ = u.Spec.ResolvedPlacement(e.Cfg.Proxmox)
	return node, prov, true
}

func (e *Engine) apply(ctx context.Context, p *Plan) error {
	// One dead node must not abort reconciliation for the whole fleet. For the
	// two maintenance ops that target an ALREADY-provisioned node — creating a
	// base template, and reconfiguring existing members — a node we cannot reach
	// can't receive the change anyway, so we warn, skip it, and press on with the
	// reachable nodes. This is the same fail-soft the vlan_tag preflight uses
	// (one down node used to abort every apply fleet-wide). Creates, scale-ups
	// and replacements are deliberately NOT skipped: asking to build something on
	// a down host is a real error that must surface.
	reach := reachCache{}

	for _, t := range p.TemplateCreates {
		prov, err := e.providerByName(t.Provider)
		if err != nil {
			return fmt.Errorf("template %s: %w", t.Name, err)
		}
		if e.nodeDown(ctx, prov, t.Node, reach) {
			e.logf("warning: skipping template %s (%s) on %s: node unreachable — a down node can't be provisioned; rerun apply once it is back", t.Name, t.Arch, t.Node)
			continue
		}
		e.logf("creating template %s (%s) on %s", t.Name, t.Arch, t.Node)
		if err := prov.EnsureTemplate(ctx, provider.TemplateSpec{
			Name:     t.Name,
			VMID:     TemplateVMID(t.Arch),
			Arch:     t.Arch,
			Node:     t.Node,
			Storage:  t.Storage,
			ImageURL: DefaultImageURL(t.Arch),
		}); err != nil {
			return fmt.Errorf("template %s: %w", t.Name, err)
		}
	}

	for _, c := range p.PoolCreates {
		if err := e.createPool(ctx, c); err != nil {
			return fmt.Errorf("create pool %s: %w", c.Name, err)
		}
	}
	for _, u := range p.Updates {
		if node, prov, ok := e.updateTarget(u); ok && e.nodeDown(ctx, prov, node, reach) {
			e.logf("warning: skipping update for pool %s on %s: node unreachable — rerun apply once it is back", u.Name, node)
			continue
		}
		if err := e.updatePool(ctx, u); err != nil {
			return fmt.Errorf("update pool %s: %w", u.Name, err)
		}
	}
	for _, r := range p.Replacements {
		if err := e.replaceMembers(ctx, r); err != nil {
			return fmt.Errorf("replace in pool %s: %w", r.Name, err)
		}
	}
	for _, s := range p.ScaleUps {
		if err := e.scaleUp(ctx, s); err != nil {
			return fmt.Errorf("scale up pool %s: %w", s.Name, err)
		}
	}
	for _, s := range p.ScaleDowns {
		if err := e.scaleDown(ctx, s); err != nil {
			return fmt.Errorf("scale down pool %s: %w", s.Name, err)
		}
	}
	for _, name := range p.PoolDestroys {
		if err := e.destroyPool(ctx, name); err != nil {
			return fmt.Errorf("destroy pool %s: %w", name, err)
		}
	}

	// Monitoring reconciles last so scrape targets reflect the final member
	// set (pools already created/scaled/destroyed above).
	e.foldTargetDrift(p)
	if p.MonitoringCreate != nil {
		if err := e.provisionMonitoring(ctx, p.MonitoringCreate.Spec, p.MonitoringCreate.Cache); err != nil {
			return fmt.Errorf("provision monitoring: %w", err)
		}
	}
	if p.MonitoringUpdate != nil {
		if err := e.updateMonitoring(ctx, p.MonitoringUpdate); err != nil {
			return fmt.Errorf("update monitoring: %w", err)
		}
	}
	if p.MonitoringDestroy {
		if err := e.destroyMonitoring(ctx); err != nil {
			return fmt.Errorf("destroy monitoring: %w", err)
		}
	}

	// The dedicated cache VM reconciles after monitoring so that a migration
	// off the utility host lands in the right order: monitoring's update stops
	// the colocated copy first, then the new VM comes up. Runner nodes pick the
	// new mirror address up on the following apply (see warnRegistryCache).
	if p.RegistryCacheCreate != nil {
		if err := e.provisionRegistryCacheVM(ctx, p.RegistryCacheCreate.Cache); err != nil {
			return fmt.Errorf("provision registry cache vm: %w", err)
		}
	}
	if p.RegistryCacheUpdate != nil {
		if err := e.updateRegistryCacheVM(ctx, p.RegistryCacheUpdate); err != nil {
			return fmt.Errorf("update registry cache vm: %w", err)
		}
	}
	if p.RegistryCacheDestroy {
		if err := e.destroyRegistryCacheVM(ctx); err != nil {
			return fmt.Errorf("destroy registry cache vm: %w", err)
		}
	}

	// Destroy before create: when the passthrough set changed, the plan has
	// both, and the old VM must release the GPU before the new one claims it.
	if p.ModelServerDestroy {
		if err := e.destroyModelServerVM(ctx); err != nil {
			return fmt.Errorf("destroy model server vm: %w", err)
		}
	}
	if p.ModelServerCreate != nil {
		if err := e.provisionModelServerVM(ctx, p.ModelServerCreate.Server); err != nil {
			return fmt.Errorf("provision model server vm: %w", err)
		}
	}
	if p.ModelServerUpdate != nil {
		if err := e.updateModelServerVM(ctx, p.ModelServerUpdate); err != nil {
			return fmt.Errorf("update model server vm: %w", err)
		}
	}
	return nil
}

// DefaultImageURL is the Ubuntu cloud image for the given arch.
func DefaultImageURL(arch string) string {
	return fmt.Sprintf("https://cloud-images.ubuntu.com/noble/current/noble-server-cloudimg-%s.img", arch)
}

// TemplateVMID maps arch to a fixed, well-known template vmid.
func TemplateVMID(arch string) int {
	if arch == "arm64" {
		return 9001
	}
	return 9000
}
