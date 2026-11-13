package engine

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"slices"
	"sort"
	"strings"

	"github.com/ephemerd/mayfly/internal/config"
	"github.com/ephemerd/mayfly/internal/ephemerd"
	"github.com/ephemerd/mayfly/internal/modelserver"
	"github.com/ephemerd/mayfly/internal/monitoring"
	"github.com/ephemerd/mayfly/internal/registrycache"
	"github.com/ephemerd/mayfly/internal/state"
)

// Plan is the computed difference between the TOML-declared fleet and the
// recorded state. Member drift maps to the cheapest tier that applies it:
//
//	config restart  repos, creds, secret, hostname, ephemerd version
//	power cycle     cores, memory
//	disk grow       disk_gb increase (live resize, no reboot)
//	replace         template change, disk shrink, or a vlan_tag/firewall change
//	                — the only rebuilds (NIC isolation is set at clone time)
type Plan struct {
	TemplateCreates []TemplateCreate // linux templates to auto-create

	PoolCreates  []PoolChange
	PoolDestroys []string
	ScaleUps     []ScaleChange
	ScaleDowns   []ScaleChange
	Updates      []UpdateChange // in-place work, grouped per pool
	Replacements []ScaleChange  // rolling replace, vmids affected

	// Monitoring node reconcile (at most one of create/update, or destroy).
	MonitoringCreate  *MonitoringChange
	MonitoringUpdate  *MonitoringUpdate
	MonitoringDestroy bool

	// Dedicated registry-cache VM reconcile (at most one of create/update, or
	// destroy). Only ever set when [registry_cache.vm] is present; a colocated
	// cache is still reconciled as part of the monitoring host.
	RegistryCacheCreate  *RegistryCacheChange
	RegistryCacheUpdate  *RegistryCacheUpdate
	RegistryCacheDestroy bool

	// Model-server VM reconcile. A change to its GPU passthrough set plans
	// BOTH a destroy and a create: the old VM has to release the card before
	// a new one can take it, and apply runs them in that order.
	ModelServerCreate  *ModelServerChange
	ModelServerUpdate  *ModelServerUpdate
	ModelServerDestroy bool

	Warnings []string
}

// MonitoringChange provisions the utility host: the Prometheus + Grafana node,
// plus the registry pull-through cache when Cache.Enabled.
type MonitoringChange struct {
	Spec  config.MonitoringSpec
	Cache config.RegistryCacheSpec
}

// MonitoringUpdate carries in-place monitoring-node work. PushConfig rewrites
// the stack assets (compose/prometheus/grafana) and re-ups the project;
// PushTargets only rewrites the scrape target list (Prometheus auto-reloads).
type MonitoringUpdate struct {
	Spec config.MonitoringSpec
	// Cache is the desired registry pull-through cache state. PushConfig
	// reconciles it in both directions — enabling installs and starts it,
	// disabling stops it and reclaims its blob store.
	Cache       config.RegistryCacheSpec
	VMID        int
	PushConfig  bool
	PushTargets bool
	PowerCycle  bool
	DiskGrowGB  int
	FromCores   int
	FromMemMB   int
	FromDiskGB  int
}

func (m MonitoringUpdate) any() bool {
	return m.PushConfig || m.PushTargets || m.PowerCycle || m.DiskGrowGB > 0
}

// RegistryCacheChange provisions the dedicated pull-through cache VM.
type RegistryCacheChange struct {
	Cache config.RegistryCacheSpec
}

// RegistryCacheUpdate carries in-place work on the dedicated cache VM.
// PushConfig rewrites the cache stack assets and re-ups the project.
type RegistryCacheUpdate struct {
	Cache      config.RegistryCacheSpec
	VMID       int
	PushConfig bool
	PowerCycle bool
	DiskGrowGB int
	// MoveDiskTo is the storage a vm_storage change moves the disk onto,
	// live; "" = no move. FromStorage is where it was ("" = the clone's).
	MoveDiskTo  string
	FromStorage string
	FromCores   int
	FromMemMB   int
	FromDiskGB  int
}

func (r RegistryCacheUpdate) any() bool {
	return r.PushConfig || r.PowerCycle || r.DiskGrowGB > 0 || r.MoveDiskTo != ""
}

// ModelServerChange provisions the model-server VM.
type ModelServerChange struct {
	Server config.ModelServerSpec
}

// ModelServerUpdate carries in-place work on the model-server VM.
// PushConfig rewrites its setup files and restarts Ollama.
type ModelServerUpdate struct {
	Server     config.ModelServerSpec
	VMID       int
	PushConfig bool
	PowerCycle bool
	DiskGrowGB int
	FromCores  int
	FromMemMB  int
	FromDiskGB int
}

func (u ModelServerUpdate) any() bool {
	return u.PushConfig || u.PowerCycle || u.DiskGrowGB > 0
}

// TemplateCreate identifies a linux base template to build on a specific
// PVE node. Windows templates are never auto-created (no cloud image
// exists) — a missing one fails the plan instead.
type TemplateCreate struct {
	Name    string
	Arch    string // amd64 | arm64
	Node    string
	Storage string
	// Provider names the backend the template is built on. Proxmox is the only
	// backend; empty is treated as proxmox.
	Provider config.Provider
}

type PoolChange struct {
	Name string
	Spec config.PoolSpec
}

type ScaleChange struct {
	Name  string
	Spec  config.PoolSpec
	Count int
	VMIDs []int
	Hosts []string // darwin: SSH hosts to enroll/unenroll
}

type UpdateChange struct {
	Name        string
	Spec        config.PoolSpec
	NewHostname string // non-empty when the pool FQDN changed (DNS route swap)
	Members     []MemberUpdate
}

type MemberUpdate struct {
	VMID   int
	Host   string // darwin: SSH host (VMID is 0)
	Config bool   // rewrite config.toml + PEM, restart ephemerd
	// ConfigReason says why Config is set, so plan can tell real drift from
	// bookkeeping.
	ConfigReason string
	// Upgrade moves the node from FromBinary to ToBinary (BinaryRefs). It is
	// independent of Config: a version bump alone is an upgrade, not a config
	// push + restart.
	Upgrade    bool
	FromBinary string
	ToBinary   string
	PowerCycle bool // drain, shutdown, apply cores/memory, start
	DiskGrowGB int  // >0: live-resize to this size + grow filesystem
	FromCores  int
	FromMemMB  int
	FromDiskGB int
}

func (m MemberUpdate) any() bool {
	return m.Config || m.Upgrade || m.PowerCycle || m.DiskGrowGB > 0
}

// withDrift folds a memberDrift's config and binary verdicts into m.
func (m MemberUpdate) withDrift(d memberDrift) MemberUpdate {
	m.Config, m.ConfigReason = d.Config, d.ConfigReason
	m.Upgrade, m.FromBinary, m.ToBinary = d.Upgrade, d.FromBinary, d.ToBinary
	return m
}

// restartsDaemon reports whether applying m bounces ephemerd in a way that can
// kill running jobs, i.e. whether the pool must be cordoned first. An upgrade
// alone does not: the daemon's self-upgrade drains in-process. The exception
// is a pinned binary_url, which forces the binary-push path (drain, then a
// plain service restart).
func (m MemberUpdate) restartsDaemon() bool {
	return m.Config || m.PowerCycle || (m.Upgrade && isURLRef(m.ToBinary))
}

func (p *Plan) Empty() bool {
	return len(p.TemplateCreates) == 0 &&
		len(p.PoolCreates) == 0 &&
		len(p.PoolDestroys) == 0 &&
		len(p.ScaleUps) == 0 &&
		len(p.ScaleDowns) == 0 &&
		len(p.Updates) == 0 &&
		len(p.Replacements) == 0 &&
		p.MonitoringCreate == nil &&
		p.MonitoringUpdate == nil &&
		!p.MonitoringDestroy &&
		p.RegistryCacheCreate == nil &&
		p.RegistryCacheUpdate == nil &&
		!p.RegistryCacheDestroy &&
		p.ModelServerCreate == nil &&
		p.ModelServerUpdate == nil &&
		!p.ModelServerDestroy
}

// MonitoringConfigHash fingerprints the utility host's stack: the embedded
// compose/prometheus/grafana assets plus the spec fields that shape the
// rendered .env / retention, and — when the registry pull-through cache is
// enabled — the cache assets and its settings too. Resource fields
// (cores/mem/disk) are compared as plain values, not folded into this hash.
//
// A disabled cache contributes a single fixed line, so a fleet that never
// enables one keeps exactly the hash it had before the cache existed and sees
// no spurious drift.
func MonitoringConfigHash(spec config.MonitoringSpec, cache config.RegistryCacheSpec) string {
	h := sha256.New()
	fmt.Fprintf(h, "assets=%s\n", monitoring.AssetsHash())
	fmt.Fprintf(h, "grafana_pw=%s\nretention=%d\ntemplate=%s\nmetrics_port=%d\n",
		spec.GrafanaAdminPassword, spec.RetentionDays, spec.Template, spec.MetricsPort)
	if !cache.Enabled {
		fmt.Fprintln(h, "registry_cache=off")
		return hex.EncodeToString(h.Sum(nil))[:16]
	}
	c := cache.Resolved()
	fmt.Fprintf(h, "registry_cache=on assets=%s port=%d upstream=%s ttl=%s max_gb=%d user=%s pass=%s\n",
		registrycache.AssetsHash(), c.Port, c.Upstream, c.TTL, c.MaxSizeGB, c.Username, c.Password)
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// registryMirror resolves the ephemerd registry-mirror render input for the
// fleet. It returns a zero value (renders nothing) when the cache is off or
// when its address is not knowable yet — the utility host's LAN address only
// exists in state once that VM has actually been provisioned, and mayfly never
// guesses a fleet's addressing.
func registryMirror(cfg *config.Config, st *state.Store) ephemerd.RegistryMirror {
	cache := cfg.RegistryCache
	if !cache.Enabled {
		return ephemerd.RegistryMirror{}
	}
	// Which host serves the cache decides which address the runners get. A
	// dedicated cache VM is the whole point of the split: its IP is on the
	// runners' VLAN, where the utility host's LAN address was unreachable.
	var hostIP string
	switch {
	case cache.Dedicated():
		if st != nil && st.RegistryCache != nil {
			hostIP = st.RegistryCache.IP
		}
	case st != nil && st.Monitoring != nil:
		hostIP = st.Monitoring.IP
	}
	endpoint := cache.MirrorEndpoint(hostIP)
	if endpoint == "" {
		return ephemerd.RegistryMirror{}
	}
	return ephemerd.NewRegistryMirror(endpoint, cache.ResolvedFailOpen())
}

// actionsCache resolves a pool's [actions_cache] render input. Zero (renders
// nothing) unless the pool opted in, the cache VM runs the Actions registry,
// and that VM's address is recorded — like the mirror, mayfly never guesses an
// address. Because the result feeds renderForHash, opting a pool in (or the
// cache VM's IP or the password changing) is config drift for its members.
func actionsCache(cfg *config.Config, st *state.Store, spec config.PoolSpec, osName string) ephemerd.ActionsCache {
	cache := cfg.RegistryCache
	if !spec.ActionsCache || !cache.ActionsActive() || (osName != "" && osName != "linux") {
		return ephemerd.ActionsCache{}
	}
	if st == nil || st.RegistryCache == nil {
		return ephemerd.ActionsCache{}
	}
	endpoint := cache.ActionsEndpoint(st.RegistryCache.IP)
	if endpoint == "" {
		return ephemerd.ActionsCache{}
	}
	a := cache.Actions.Resolved()
	return ephemerd.ActionsCache{
		Registry: endpoint,
		Username: a.Username,
		Password: a.ResolvedPassword(),
	}
}

// ConfigHash fingerprints everything delivered to the node as files: the
// rendered ephemerd config (which embeds repos, creds, hostname, secret,
// token) and the GitHub App PEM.
//
// It deliberately does NOT cover the ephemerd binary. It used to, and that
// made a version bump indistinguishable from a config change: bumping
// [ephemerd] version and then upgrading every node with `mayfly node upgrade`
// left every member's hash stale, so plan offered to config-push and restart
// the whole fleet, cordoning each pool's webhooks for the length of its drain,
// to "fix" nothing. The binary is now recorded in state.Member.Ephemerd and
// diffed on its own (see diffMemberEphemerd).
//
// The value carries a "c2:" prefix so a hash written by the old formula is
// recognisable and can still be checked (legacyConfigHash) rather than
// treated as drift on the first plan after an upgrade of mayfly itself.
func ConfigHash(cfg *config.Config, st *state.Store, spec config.PoolSpec, pool *state.Pool, pem []byte) string {
	h := sha256.New()
	h.Write([]byte(renderForHash(cfg, st, spec, pool)))
	h.Write(pem)
	return configHashPrefix + hex.EncodeToString(h.Sum(nil))[:16]
}

// configHashPrefix marks a ConfigHash that excludes the binary selection.
const configHashPrefix = "c2:"

// legacyConfigHash reproduces the pre-"c2:" fingerprint, which folded the
// global [ephemerd] version and binary_url into the hash. version and url are
// the binary selection to test the recorded hash against.
func legacyConfigHash(cfg *config.Config, st *state.Store, spec config.PoolSpec, pool *state.Pool, pem []byte, version, url string) string {
	h := sha256.New()
	h.Write([]byte(renderForHash(cfg, st, spec, pool)))
	fmt.Fprintf(h, "\nbinary=%s|%s\n", version, url)
	h.Write(pem)
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// renderForHash renders the ephemerd config.toml a member of pool would get,
// for fingerprinting.
func renderForHash(cfg *config.Config, st *state.Store, spec config.PoolSpec, pool *state.Pool) string {
	rendered, err := ephemerd.RenderConfig(ephemerd.ConfigInputs{
		Owner:                 cfg.GitHub.Owner,
		Repos:                 spec.Repos,
		AppID:                 cfg.GitHub.AppID,
		InstallationID:        cfg.GitHub.InstallationID,
		MaxConcurrent:         spec.MaxConcurrent,
		JobTimeout:            spec.JobTimeout,
		OrphanGrace:           spec.OrphanGrace,
		PrivateKeyPath:        pemPath(spec.OS),
		TunnelToken:           pool.Token,
		TunnelHostname:        spec.Hostname,
		WebhookSecret:         pool.Secret,
		MetricsEnabled:        cfg.MetricsEnabled(),
		MetricsPort:           cfg.MetricsPort(),
		MetricsBindAddr:       cfg.MetricsBindAddr(),
		DefaultImageLinux:     cfg.GitHub.DefaultImageLinux,
		DefaultImageWindows:   cfg.GitHub.DefaultImageWindows,
		DispatchPolicy:        dispatchPolicy(spec.ResolvedDispatchPolicy(cfg.GitHub)),
		DindEnabled:           cfg.Dind.Enabled,
		DindAllowPrivileged:   cfg.Dind.AllowPrivileged,
		RegistryMirror:        registryMirror(cfg, st),
		OS:                    spec.OS,
		ModuleProxyEnabled:    cfg.ModuleProxy.Enabled,
		CargoProxyEnabled:     cfg.CargoProxy.Enabled,
		ComposerProxyEnabled:  cfg.ComposerProxy.Enabled,
		NpmProxyEnabled:       cfg.NpmProxy.Enabled,
		PipProxyEnabled:       cfg.PipProxy.Enabled,
		PubProxyEnabled:       cfg.PubProxy.Enabled,
		GhrelProxyEnabled:     cfg.GhrelProxy.Enabled,
		ActionsCache:          actionsCache(cfg, st, spec, spec.OS),
		ExtraTargets:          extraTargets(spec, cfg.GitHub),
		MacOSVM:               vmResources(spec.VM.MacOS),
		LinuxVM:               vmResources(spec.VM.Linux),
		WindowsRunnerMemoryMB: spec.WindowsRunner.MemoryMB,
		WindowsRunnerCPUs:     spec.WindowsRunner.CPUs,
	})
	if err != nil {
		// Invalid render inputs surface properly during apply; for hashing
		// purposes an error sentinel just means "differs from everything".
		rendered = "invalid:" + err.Error()
	}
	return rendered
}

// extraTargets converts a pool's secondary GitHub targets into render inputs,
// resolving each PAT from its config field or named environment variable.
// Each target also carries its own dispatch allowlist: ephemerd applies a
// policy per target with no inheritance, so gh supplies the pool's effective
// policy for it (labels inherited, repo allowlist never — see
// config.PoolSpec.EffectiveDispatchPolicyFor).
func extraTargets(spec config.PoolSpec, gh config.GitHub) []ephemerd.ExtraTarget {
	if len(spec.GitHubExtra) == 0 {
		return nil
	}
	out := make([]ephemerd.ExtraTarget, 0, len(spec.GitHubExtra))
	for _, t := range spec.GitHubExtra {
		out = append(out, ephemerd.ExtraTarget{
			Owner:          t.Owner,
			Repos:          t.Repos,
			Token:          t.ResolvedToken(),
			DispatchPolicy: dispatchPolicy(spec.EffectiveDispatchPolicyFor(t, gh)),
		})
	}
	return out
}

// dispatchPolicy maps a config dispatch allowlist to the ephemerd render input.
func dispatchPolicy(d config.DispatchPolicy) ephemerd.DispatchPolicy {
	return ephemerd.DispatchPolicy{
		AllowedRepos:   d.AllowedRepos,
		RequiredLabels: d.RequiredLabels,
	}
}

// vmResources maps a config VM sizing spec to the ephemerd render input.
func vmResources(v config.VMSpec) ephemerd.VMResources {
	return ephemerd.VMResources{
		Enabled:    v.Enabled,
		CPUs:       v.CPUs,
		MemoryMB:   v.MemoryMB,
		DiskSizeGB: v.DiskSizeGB,
	}
}

// Diff computes the plan. pem is the GitHub App private key content (part of
// the config fingerprint); missingTemplates is determined against live PVE
// by the caller (linux only — missing windows templates error before Diff).
// forceReplace names a pool whose members are rebuilt regardless.
func Diff(cfg *config.Config, st *state.Store, pem []byte, missingTemplates []TemplateCreate, forceReplace string) *Plan {
	p := &Plan{TemplateCreates: missingTemplates}

	names := make([]string, 0, len(cfg.Pools))
	for name := range cfg.Pools {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, name := range names {
		spec := cfg.Pools[name].Resolved(cfg.Defaults)

		cur, ok := st.FindPool(name)
		if !ok {
			p.PoolCreates = append(p.PoolCreates, PoolChange{Name: name, Spec: spec})
			continue
		}

		desiredCfgHash := ConfigHash(cfg, st, spec, cur, pem)
		update := UpdateChange{Name: name, Spec: spec}
		if cur.Hostname != spec.Hostname {
			update.NewHostname = spec.Hostname
		}

		// Enrolled pools (darwin, or linux with members): bare metal, declared
		// member list. Reconcile by host set (enroll/unenroll) plus config-tier
		// updates; there is no VM-resource or replace tier.
		if spec.UsesEnroll() {
			declared := map[string]bool{}
			for _, h := range spec.Members {
				declared[h] = true
			}
			enrolled := map[string]bool{}
			for _, m := range cur.Members {
				enrolled[m.Hostname] = true
				if !declared[m.Hostname] {
					p.ScaleDowns = append(p.ScaleDowns, ScaleChange{Name: name, Spec: spec, Hosts: []string{m.Hostname}})
					continue
				}
				d := diffMemberEphemerd(cfg, st, spec, cur, pem, m, desiredCfgHash)
				if forceReplace == name {
					d.Config, d.ConfigReason = true, "--replace"
				}
				if mu := (MemberUpdate{Host: m.Hostname}).withDrift(d); mu.any() {
					update.Members = append(update.Members, mu)
				}
			}
			var adds []string
			for _, h := range spec.Members {
				if !enrolled[h] {
					adds = append(adds, h)
				}
			}
			if len(adds) > 0 {
				p.ScaleUps = append(p.ScaleUps, ScaleChange{Name: name, Spec: spec, Count: len(adds), Hosts: adds})
			}
			if len(update.Members) > 0 || update.NewHostname != "" {
				p.Updates = append(p.Updates, update)
			}
			continue
		}

		var replace []int
		live := 0
		for _, m := range cur.Members {
			if forceReplace == name ||
				m.Template != spec.Template ||
				m.OS != spec.OS || m.Arch != spec.Arch ||
				m.VLANTag != spec.VLANTag || m.Firewall != spec.Firewall ||
				m.VMStorage != spec.VMStorage ||
				(spec.DiskGB > 0 && m.DiskGB > spec.DiskGB) {
				replace = append(replace, m.VMID)
				continue
			}
			live++

			mu := MemberUpdate{
				VMID:       m.VMID,
				FromCores:  m.Cores,
				FromMemMB:  m.MemoryMB,
				FromDiskGB: m.DiskGB,
			}
			mu = mu.withDrift(diffMemberEphemerd(cfg, st, spec, cur, pem, m, desiredCfgHash))
			if m.Cores != spec.Cores || m.MemoryMB != spec.MemoryMB {
				mu.PowerCycle = true
			}
			if spec.DiskGB > m.DiskGB {
				mu.DiskGrowGB = spec.DiskGB
			}
			if mu.any() {
				update.Members = append(update.Members, mu)
			}
		}

		if len(replace) > 0 {
			p.Replacements = append(p.Replacements, ScaleChange{Name: name, Spec: spec, VMIDs: replace})
		}
		if len(update.Members) > 0 || update.NewHostname != "" {
			p.Updates = append(p.Updates, update)
		}

		total := live + len(replace)
		switch {
		case total < spec.Size:
			p.ScaleUps = append(p.ScaleUps, ScaleChange{Name: name, Spec: spec, Count: spec.Size - total})
		case total > spec.Size:
			excess := total - spec.Size
			vmids := make([]int, 0, excess)
			for i := len(cur.Members) - 1; i >= 0 && len(vmids) < excess; i-- {
				vmids = append(vmids, cur.Members[i].VMID)
			}
			p.ScaleDowns = append(p.ScaleDowns, ScaleChange{Name: name, Spec: spec, VMIDs: vmids})
		}
	}

	for _, pool := range st.Pools {
		if _, ok := cfg.Pools[pool.Name]; !ok {
			p.PoolDestroys = append(p.PoolDestroys, pool.Name)
		}
	}
	sort.Strings(p.PoolDestroys)

	diffMonitoring(cfg, st, p)
	diffRegistryCacheVM(cfg, st, p)
	diffModelServerVM(cfg, st, p)
	warnRegistryCache(cfg, st, p)
	warnFirewall(cfg, p)

	return p
}

// warnFirewall makes it loud that firewall=true on a pool enforces NOTHING on
// its own. mayfly writes "firewall=1" on the runner vNIC, but with the Proxmox
// firewall disabled at the datacenter/VM level (the default) that flag is
// inert: no rules are evaluated and the runner reaches the whole LAN exactly as
// if the knob were off. The operator is rolling this out for untrusted
// public-repo CI, so a setting that looks like isolation but silently does
// nothing is worse than none — say so on every plan while it is set, rather
// than let it read as enforced. Enabling the datacenter+VM firewall and its
// default-deny policy is a PVE-side configuration mayfly does not manage.
func warnFirewall(cfg *config.Config, p *Plan) {
	var pools []string
	for name, ps := range cfg.Pools {
		if ps.Firewall {
			pools = append(pools, name)
		}
	}
	if len(pools) == 0 {
		return
	}
	sort.Strings(pools)
	p.Warnings = append(p.Warnings,
		fmt.Sprintf("firewall=true is set on pool(s) %v but only writes firewall=1 on the runner vNIC — "+
			"it enforces NOTHING until the Proxmox firewall is enabled at the datacenter and VM level "+
			"with a default-deny input policy (configured outside mayfly). "+
			"Until then these runners reach the whole LAN as if firewall were off.", pools))
}

// vlanBridgeError is the decision half of the vlan_tag preflight, split out so
// it can be table-tested without a live Proxmox. It returns a non-nil,
// actionable error only for the silent fail-open case: a pool that requests a
// VLAN tag whose resolved host bridge is NOT VLAN-aware. Proxmox writes net0
// "tag=<n>" onto such a bridge and boots the VM, but the guest never reaches
// the VLAN — it stays on the untagged LAN while looking isolated. A pool with
// no tag (vlanTag <= 0) or a VLAN-aware bridge returns nil.
func vlanBridgeError(pool string, vlanTag int, node, bridge string, vlanAware bool) error {
	if vlanTag <= 0 || vlanAware {
		return nil
	}
	return fmt.Errorf("pool %s sets vlan_tag=%d but bridge %s on node %s is not VLAN-aware; "+
		"enable it on the Proxmox host (set 'bridge-vlan-aware yes' on %s in /etc/network/interfaces "+
		"and run `ifreload -a`, or tick the bridge's \"VLAN aware\" box in the PVE UI), then retry — "+
		"until then the tag is written but the runner stays on the untagged LAN, not VLAN %d",
		pool, vlanTag, bridge, node, bridge, vlanTag)
}

// warnRegistryCache flags the one-apply lag that exists when the cache is
// enabled in the same run that first creates the host it runs on: the mirror
// URL is derived from that host's address, which does not exist until the VM
// is up and the plan was computed before that, so runner nodes pick the mirror
// up on the following apply. Setting registry_cache.endpoint explicitly avoids
// the lag entirely.
func warnRegistryCache(cfg *config.Config, st *state.Store, p *Plan) {
	if !cfg.RegistryCache.Enabled || registryMirror(cfg, st).Enabled() {
		return
	}
	host := "the utility host"
	if cfg.RegistryCache.Dedicated() {
		host = "the dedicated cache VM"
	}
	p.Warnings = append(p.Warnings,
		"registry_cache is enabled but "+host+" has no address in state yet — "+
			"runner nodes will be pointed at the cache on the next apply "+
			"(set registry_cache.endpoint to skip the wait)")
}

// diffMonitoring reconciles the single monitoring node. Targets are computed
// against the DESIRED post-apply member set is not possible here (state still
// reflects pre-apply), so target drift is judged against current state; the
// engine regenerates targets from final state when it actually pushes.
func diffMonitoring(cfg *config.Config, st *state.Store, p *Plan) {
	switch {
	case cfg.Monitoring == nil && st.Monitoring != nil:
		p.MonitoringDestroy = true
	case cfg.Monitoring == nil:
		// no monitoring configured and none recorded: nothing to do
	case st.Monitoring == nil:
		p.MonitoringCreate = &MonitoringChange{Spec: cfg.Monitoring.Resolved(), Cache: cfg.RegistryCache.OnMonitoring()}
	default:
		spec := cfg.Monitoring.Resolved()
		cur := st.Monitoring
		u := MonitoringUpdate{
			Spec:       spec,
			Cache:      cfg.RegistryCache.OnMonitoring(),
			VMID:       cur.VMID,
			FromCores:  cur.Cores,
			FromMemMB:  cur.MemoryMB,
			FromDiskGB: cur.DiskGB,
		}
		if MonitoringConfigHash(spec, cfg.RegistryCache.OnMonitoring()) != cur.ConfigHash {
			u.PushConfig = true
		}
		if monitoring.TargetsHash(st, spec.MetricsPort) != cur.TargetHash {
			u.PushTargets = true
		}
		if cur.Cores != spec.Cores || cur.MemoryMB != spec.MemoryMB {
			u.PowerCycle = true
		}
		if spec.DiskGB > cur.DiskGB {
			u.DiskGrowGB = spec.DiskGB
		}
		if u.any() {
			p.MonitoringUpdate = &u
		}
	}
}

// diffRegistryCacheVM reconciles the dedicated pull-through cache VM.
//
// The migration case matters most: a fleet that has been running a colocated
// cache and now declares [registry_cache.vm] gets a create here, while
// diffMonitoring independently sees the cache go away from the utility host
// (its Cache is routed through OnMonitoring). One apply therefore both stands
// the new VM up and stops the old copy, instead of leaving two caches running.
func diffRegistryCacheVM(cfg *config.Config, st *state.Store, p *Plan) {
	cache := cfg.RegistryCache
	switch {
	case !cache.Dedicated() && st.RegistryCache != nil:
		// Either the cache was turned off, or it moved back onto the utility
		// host. Both mean this VM should go.
		p.RegistryCacheDestroy = true
	case !cache.Dedicated():
		// Not dedicated and none recorded: nothing to do.
	case st.RegistryCache == nil:
		p.RegistryCacheCreate = &RegistryCacheChange{Cache: cache}
	default:
		vm := cache.VM.Resolved()
		cur := st.RegistryCache
		u := RegistryCacheUpdate{
			Cache:      cache,
			VMID:       cur.VMID,
			FromCores:  cur.Cores,
			FromMemMB:  cur.MemoryMB,
			FromDiskGB: cur.DiskGB,
		}
		if RegistryCacheConfigHash(cache) != cur.ConfigHash {
			u.PushConfig = true
		}
		if cur.Cores != vm.Cores || cur.MemoryMB != vm.MemoryMB {
			u.PowerCycle = true
		}
		if vm.DiskGB > cur.DiskGB {
			u.DiskGrowGB = vm.DiskGB
		}
		// Only a move TO a named storage. Clearing vm_storage leaves the disk
		// where it is: moving it back onto the template's (small) storage is
		// never what an operator deleting the line meant.
		if vm.VMStorage != "" && vm.VMStorage != cur.VMStorage {
			u.MoveDiskTo, u.FromStorage = vm.VMStorage, cur.VMStorage
		}
		if u.any() {
			p.RegistryCacheUpdate = &u
		}
	}
}

// diffModelServerVM reconciles the model-server VM.
//
// The passthrough set is the one field that cannot change in place: moving a
// GPU means the old VM must give the card up before a new one is created with
// it, so a changed hostpci list plans a destroy AND a create (apply orders
// them). Everything else follows the registry cache VM's rules.
func diffModelServerVM(cfg *config.Config, st *state.Store, p *Plan) {
	m := cfg.ModelServer
	cur := st.ModelServer
	switch {
	case !m.Active() && cur != nil:
		p.ModelServerDestroy = true
	case !m.Active():
	case cur == nil:
		p.ModelServerCreate = &ModelServerChange{Server: m}
	case !slices.Equal(cur.HostPCI, m.VM.HostPCI):
		p.ModelServerDestroy = true
		p.ModelServerCreate = &ModelServerChange{Server: m}
	default:
		vm := m.VM.Resolved()
		u := ModelServerUpdate{
			Server:     m,
			VMID:       cur.VMID,
			FromCores:  cur.Cores,
			FromMemMB:  cur.MemoryMB,
			FromDiskGB: cur.DiskGB,
		}
		if ModelServerConfigHash(m) != cur.ConfigHash {
			u.PushConfig = true
		}
		if cur.Cores != vm.Cores || cur.MemoryMB != vm.MemoryMB {
			u.PowerCycle = true
		}
		if vm.DiskGB > cur.DiskGB {
			u.DiskGrowGB = vm.DiskGB
		}
		if u.any() {
			p.ModelServerUpdate = &u
		}
	}
	if m.Active() && strings.Contains(cfg.Proxmox.TokenID, "!") {
		for _, d := range m.VM.HostPCI {
			if config.IsPCIAddress(d) {
				p.Warnings = append(p.Warnings, fmt.Sprintf(
					"model_server.vm.hostpci %q is a raw PCI address, but mayfly authenticates with an API token (%s); Proxmox only lets root@pam itself pass a raw address through. Create a Datacenter → Resource Mappings → PCI entry for the card and list its name instead.",
					d, cfg.Proxmox.TokenID))
			}
		}
	}
}

// Render prints the plan in a terraform-ish format.
func (p *Plan) Render(w io.Writer) {

	for _, t := range p.TemplateCreates {
		fmt.Fprintf(w, "  + template.%s (%s on %s: download cloud image, import, convert)\n", t.Name, t.Arch, t.Node)
	}
	for _, c := range p.PoolCreates {
		fmt.Fprintf(w, "  + pool.%s\n", c.Name)
		fmt.Fprintf(w, "      hostname: %s\n", c.Spec.Hostname)
		fmt.Fprintf(w, "      tunnel + DNS + webhook secret\n")
		if c.Spec.UsesEnroll() {
			fmt.Fprintf(w, "      os/arch:  %s/%s (bare metal via SSH)\n", c.Spec.OS, c.Spec.Arch)
			fmt.Fprintf(w, "      enroll:   %v\n", c.Spec.Members)
		} else {
			fmt.Fprintf(w, "      os/arch:  %s/%s (template %s)\n", c.Spec.OS, c.Spec.Arch, c.Spec.Template)
			fmt.Fprintf(w, "      members:  %d (cores=%d mem=%dMB disk=%dGB)\n", c.Spec.Size, c.Spec.Cores, c.Spec.MemoryMB, c.Spec.DiskGB)
		}
	}
	for _, u := range p.Updates {
		fmt.Fprintf(w, "  ~ pool.%s\n", u.Name)
		if u.NewHostname != "" {
			fmt.Fprintf(w, "      hostname → %s (DNS route swap)\n", u.NewHostname)
		}
		for _, m := range u.Members {
			var acts []string
			if m.Config {
				act := "update config + restart ephemerd"
				if m.Host != "" {
					act = "re-enroll (config + binary) + restart ephemerd"
				}
				if m.ConfigReason != "" {
					act += " (" + m.ConfigReason + ")"
				}
				acts = append(acts, act)
			}
			if m.Upgrade && !(m.Host != "" && m.Config) {
				// An enrolled member being re-enrolled gets the new binary as
				// part of that; only a standalone upgrade is its own action.
				how := "self-upgrade; drains in-daemon, no webhook cordon"
				if isURLRef(m.ToBinary) {
					how = "binary push of the pinned binary_url; pool cordoned"
				}
				acts = append(acts, fmt.Sprintf("upgrade ephemerd %s → %s (%s)",
					describeBinary(m.FromBinary), describeBinary(m.ToBinary), how))
			} else if m.Upgrade {
				acts = append(acts, fmt.Sprintf("ephemerd %s → %s", describeBinary(m.FromBinary), describeBinary(m.ToBinary)))
			}
			id := fmt.Sprintf("vmid %d", m.VMID)
			if m.Host != "" {
				id = m.Host + " (ssh)"
			}
			if m.PowerCycle {
				acts = append(acts, fmt.Sprintf("power cycle (cores %d→%d, mem %d→%dMB)",
					m.FromCores, u.Spec.Cores, m.FromMemMB, u.Spec.MemoryMB))
			}
			if m.DiskGrowGB > 0 {
				acts = append(acts, fmt.Sprintf("grow disk %dG→%dG (live)", m.FromDiskGB, m.DiskGrowGB))
			}
			fmt.Fprintf(w, "      %s: %s\n", id, join(acts))
		}
	}
	for _, s := range p.ScaleUps {
		if len(s.Hosts) > 0 {
			fmt.Fprintf(w, "  ~ pool.%s              enroll %v via SSH\n", s.Name, s.Hosts)
		} else {
			fmt.Fprintf(w, "  ~ pool.%s              +%d member(s)\n", s.Name, s.Count)
		}
	}
	for _, s := range p.ScaleDowns {
		if len(s.Hosts) > 0 {
			fmt.Fprintf(w, "  ~ pool.%s              unenroll %v (uninstall ephemerd, host untouched)\n", s.Name, s.Hosts)
		} else {
			fmt.Fprintf(w, "  ~ pool.%s              -%d member(s) %v\n", s.Name, len(s.VMIDs), s.VMIDs)
		}
	}
	for _, r := range p.Replacements {
		fmt.Fprintf(w, "  ± pool.%s              REPLACE %d member(s) %v (template change, disk shrink, vlan_tag/firewall change, or --replace)\n", r.Name, len(r.VMIDs), r.VMIDs)
	}
	for _, name := range p.PoolDestroys {
		fmt.Fprintf(w, "  - pool.%s              destroy all members, tunnel, DNS\n", name)
	}

	if c := p.MonitoringCreate; c != nil {
		fmt.Fprintf(w, "  + monitoring\n")
		fmt.Fprintf(w, "      Prometheus + Grafana node (%s on %s)\n", c.Spec.Template, "proxmox")
		fmt.Fprintf(w, "      resources: cores=%d mem=%dMB disk=%dGB\n", c.Spec.Cores, c.Spec.MemoryMB, c.Spec.DiskGB)
		fmt.Fprintf(w, "      scrapes every pool member :%d over the LAN; Grafana on :3000\n", c.Spec.MetricsPort)
		if cache := c.Cache; cache.Enabled {
			r := cache.Resolved()
			fmt.Fprintf(w, "      registry pull-through cache on :%d → %s (cap %dGB, ttl %s)\n",
				r.Port, r.Upstream, r.MaxSizeGB, r.TTL)
		}
	}
	if u := p.MonitoringUpdate; u != nil {
		fmt.Fprintf(w, "  ~ monitoring (vmid %d)\n", u.VMID)
		var acts []string
		if u.PushConfig {
			act := "update stack config + recompose"
			if u.Cache.Enabled {
				r := u.Cache.Resolved()
				act += fmt.Sprintf(" (incl. registry cache on :%d → %s, cap %dGB)", r.Port, r.Upstream, r.MaxSizeGB)
			}
			acts = append(acts, act)
		}
		if u.PushTargets {
			acts = append(acts, "refresh scrape targets")
		}
		if u.PowerCycle {
			acts = append(acts, fmt.Sprintf("power cycle (cores %d→%d, mem %d→%dMB)",
				u.FromCores, u.Spec.Cores, u.FromMemMB, u.Spec.MemoryMB))
		}
		if u.DiskGrowGB > 0 {
			acts = append(acts, fmt.Sprintf("grow disk %dG→%dG (live)", u.FromDiskGB, u.DiskGrowGB))
		}
		fmt.Fprintf(w, "      %s\n", join(acts))
	}
	if p.MonitoringDestroy {
		fmt.Fprintf(w, "  - monitoring              destroy Prometheus + Grafana node\n")
	}

	if c := p.RegistryCacheCreate; c != nil {
		vm := c.Cache.VM.Resolved()
		r := c.Cache.Resolved()
		fmt.Fprintf(w, "  + registry_cache\n")
		fmt.Fprintf(w, "      dedicated pull-through cache VM (%s)\n", vm.Template)
		fmt.Fprintf(w, "      resources: cores=%d mem=%dMB disk=%dGB\n", vm.Cores, vm.MemoryMB, vm.DiskGB)
		if vm.VLANTag > 0 {
			fmt.Fprintf(w, "      vlan %d — reachable by the runners on their own VLAN\n", vm.VLANTag)
		} else {
			fmt.Fprintf(w, "      UNTAGGED vnic — runners on an isolated VLAN will NOT reach it\n")
		}
		fmt.Fprintf(w, "      serves :%d → %s (cap %dGB, ttl %s)\n", r.Port, r.Upstream, r.MaxSizeGB, r.TTL)
	}
	if u := p.RegistryCacheUpdate; u != nil {
		vm := u.Cache.VM.Resolved()
		fmt.Fprintf(w, "  ~ registry_cache (vmid %d)\n", u.VMID)
		var acts []string
		if u.PushConfig {
			r := u.Cache.Resolved()
			acts = append(acts, fmt.Sprintf("update cache config + recompose (:%d → %s, cap %dGB)", r.Port, r.Upstream, r.MaxSizeGB))
		}
		if u.PowerCycle {
			acts = append(acts, fmt.Sprintf("power cycle (cores %d→%d, mem %d→%dMB)",
				u.FromCores, vm.Cores, u.FromMemMB, vm.MemoryMB))
		}
		if u.MoveDiskTo != "" {
			from := u.FromStorage
			if from == "" {
				from = "clone storage"
			}
			acts = append(acts, fmt.Sprintf("move disk %s→%s (live, VM and IP kept)", from, u.MoveDiskTo))
		}
		if u.DiskGrowGB > 0 {
			acts = append(acts, fmt.Sprintf("grow disk %dG→%dG (live)", u.FromDiskGB, u.DiskGrowGB))
		}
		if u.PushConfig && u.Cache.ActionsActive() {
			a := u.Cache.Actions.Resolved()
			acts = append(acts, fmt.Sprintf("actions registry :%d (cap %dGB)", a.Port, a.MaxSizeGB))
		}
		fmt.Fprintf(w, "      %s\n", join(acts))
	}
	if p.RegistryCacheDestroy {
		fmt.Fprintf(w, "  - registry_cache          destroy dedicated pull-through cache VM\n")
	}

	if p.ModelServerDestroy {
		fmt.Fprintf(w, "  - model_server            destroy model-server VM (releases its GPU)\n")
	}
	if c := p.ModelServerCreate; c != nil {
		vm := c.Server.VM.Resolved()
		r := c.Server.Resolved()
		fmt.Fprintf(w, "  + model_server\n")
		fmt.Fprintf(w, "      GPU model-server VM (%s), hostpci %v\n", vm.Template, vm.HostPCI)
		fmt.Fprintf(w, "      resources: cores=%d mem=%dMB (pinned: passthrough disables ballooning) disk=%dGB\n", vm.Cores, vm.MemoryMB, vm.DiskGB)
		if vm.VLANTag > 0 {
			fmt.Fprintf(w, "      vlan %d\n", vm.VLANTag)
		}
		fmt.Fprintf(w, "      ollama on :%d (OpenAI-compatible /v1), models %v, NVIDIA driver %s\n", r.Port, r.Models, modelserver.DriverBranch)
	}
	if u := p.ModelServerUpdate; u != nil {
		vm := u.Server.VM.Resolved()
		fmt.Fprintf(w, "  ~ model_server (vmid %d)\n", u.VMID)
		var acts []string
		if u.PushConfig {
			r := u.Server.Resolved()
			acts = append(acts, fmt.Sprintf("update setup + restart ollama (:%d, models %v)", r.Port, r.Models))
		}
		if u.PowerCycle {
			acts = append(acts, fmt.Sprintf("power cycle (cores %d→%d, mem %d→%dMB)",
				u.FromCores, vm.Cores, u.FromMemMB, vm.MemoryMB))
		}
		if u.DiskGrowGB > 0 {
			acts = append(acts, fmt.Sprintf("grow disk %dG→%dG (live)", u.FromDiskGB, u.DiskGrowGB))
		}
		fmt.Fprintf(w, "      %s\n", join(acts))
	}

	if p.Empty() {
		fmt.Fprintln(w, "No changes. Fleet matches configuration.")
		return
	}
	adds, changes, destroys := p.Counts()
	fmt.Fprintf(w, "\nPlan: %d to add, %d to change, %d to destroy.\n", adds, changes, destroys)
	for _, warn := range p.Warnings {
		fmt.Fprintf(w, "\nWarning: %s\n", warn)
	}
}

func join(parts []string) string {
	out := ""
	for i, s := range parts {
		if i > 0 {
			out += "; "
		}
		out += s
	}
	return out
}

// Counts tallies the plan the way Render's summary line does: one add per
// template/pool/monitoring/cache create, one change per pool update, scale,
// replacement or monitoring/cache update, one destroy per teardown.
func (p *Plan) Counts() (adds, changes, destroys int) {
	b := func(v bool) int {
		if v {
			return 1
		}
		return 0
	}
	adds = len(p.TemplateCreates) + len(p.PoolCreates) +
		b(p.MonitoringCreate != nil) + b(p.RegistryCacheCreate != nil) + b(p.ModelServerCreate != nil)
	changes = len(p.Updates) + len(p.ScaleUps) + len(p.ScaleDowns) + len(p.Replacements) +
		b(p.MonitoringUpdate != nil) + b(p.RegistryCacheUpdate != nil) + b(p.ModelServerUpdate != nil)
	destroys = len(p.PoolDestroys) + b(p.MonitoringDestroy) + b(p.RegistryCacheDestroy) + b(p.ModelServerDestroy)
	return adds, changes, destroys
}
