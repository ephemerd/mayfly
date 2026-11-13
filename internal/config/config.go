package config

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

// Provider names a hypervisor backend a pool can be provisioned on.
type Provider = string

const (
	// ProviderProxmox is the only supported backend (Proxmox VE). It remains a
	// named constant (rather than an implicit default) so existing
	// provider = "proxmox" configs keep parsing.
	ProviderProxmox Provider = "proxmox"
)

type Config struct {
	Proxmox    Proxmox             `toml:"proxmox"`
	Cloudflare Cloudflare          `toml:"cloudflare"`
	GitHub     GitHub              `toml:"github"`
	Ephemerd   Ephemerd            `toml:"ephemerd"`
	Defaults   Defaults            `toml:"defaults"`
	Pools      map[string]PoolSpec `toml:"pools"`
	// Dind and the *Proxy toggles are fleet-wide ephemerd settings rendered
	// into every node's config.toml. Dind gives jobs an in-daemon Docker
	// (BuildKit); the proxies are per-runner package caches (Go modules, Rust
	// crates, PHP Packagist, npm, PyPI, Dart pub, GitHub release assets). Each
	// requires an ephemerd build carrying that proxy — ephemerd silently ignores
	// TOML sections it doesn't know, so enabling one against an older daemon is a
	// no-op rather than an error. All are skipped on windows pools (they listen
	// on the bridge gateway, which Hyper-V-isolated job containers can't reach).
	Dind          Dind        `toml:"dind"`
	ModuleProxy   ModuleProxy `toml:"module_proxy"`
	CargoProxy    CacheToggle `toml:"cargo_proxy"`
	ComposerProxy CacheToggle `toml:"composer_proxy"`
	NpmProxy      CacheToggle `toml:"npm_proxy"`
	PipProxy      CacheToggle `toml:"pip_proxy"`
	PubProxy      CacheToggle `toml:"pub_proxy"`
	GhrelProxy    CacheToggle `toml:"ghrel_proxy"`
	// Monitoring, when present, provisions a single long-lived Prometheus +
	// Grafana node and turns on ephemerd's /metrics endpoint on every runner
	// so the node can scrape them over the LAN.
	Monitoring *MonitoringSpec `toml:"monitoring"`
	// RegistryCache is a container-registry pull-through cache hosted on that
	// same VM (the "utility host"). Off unless enabled; it needs [monitoring]
	// because that section is what declares the host.
	RegistryCache RegistryCacheSpec `toml:"registry_cache"`
	// ModelServer is a GPU-backed LLM server on a VM of its own, serving an
	// OpenAI-compatible API to the fleet. Off unless enabled.
	ModelServer ModelServerSpec `toml:"model_server"`
}

// MonitoringSpec declares the observability node. It is not a runner pool —
// it runs no ephemerd, registers no webhook, and never scales. A single VM
// hosts Prometheus (scrapes every pool member's :9090 via file_sd generated
// from state) and Grafana (pre-provisioned dashboards).
type MonitoringSpec struct {
	// Hostname optionally exposes Grafana externally through a cloudflared
	// tunnel (like a pool's webhook hostname). Empty = LAN-only on :3000.
	Hostname string `toml:"hostname"`
	Cores    int    `toml:"cores"`
	MemoryMB int    `toml:"memory_mb"`
	DiskGB   int    `toml:"disk_gb"`
	// Template/Node/Storage default to the linux amd64 template and the
	// global [proxmox] placement.
	Template string `toml:"template"`
	Node     string `toml:"node"`
	Storage  string `toml:"storage"`
	// GrafanaAdminPassword seeds the Grafana admin account. Empty = "admin".
	GrafanaAdminPassword string `toml:"grafana_admin_password"`
	// RetentionDays caps Prometheus TSDB retention (default 15).
	RetentionDays int `toml:"retention_days"`
	// MetricsPort is the port ephemerd exposes /metrics on for scraping
	// (default 9090).
	MetricsPort int `toml:"metrics_port"`

	// MetricsBindAddr is the interface runner nodes bind /metrics to.
	//
	// Declaring [monitoring] is NOT enough to get metrics. ephemerd binds
	// /metrics to 127.0.0.1 unless told otherwise, deliberately, because the
	// endpoint is unauthenticated. So the monitoring node scrapes, gets
	// "connection refused", and every runner target shows DOWN -- while each
	// node reports perfectly healthy, because it is.
	//
	// The fleet ran exactly like that: all five targets down, which is why a
	// registry-cache outage went unnoticed for four weeks. There were no
	// metrics to notice it with.
	//
	// Set "0.0.0.0" to let the monitoring node scrape. Empty (the default)
	// renders nothing and keeps ephemerd's loopback behaviour, so an
	// existing fleet sees no config drift from this field existing.
	//
	// The endpoint carries no auth: firewall the port to the monitoring
	// host, and/or set ephemerd's metrics tls_cert/tls_key, when opening it.
	MetricsBindAddr string `toml:"metrics_bind_addr"`
}

// Resolved fills zero fields with defaults.
func (m MonitoringSpec) Resolved() MonitoringSpec {
	if m.Cores == 0 {
		m.Cores = 2
	}
	if m.MemoryMB == 0 {
		m.MemoryMB = 4096
	}
	if m.DiskGB == 0 {
		m.DiskGB = 40
	}
	if m.Template == "" {
		m.Template = "mayfly-ubuntu-24.04-amd64"
	}
	if m.RetentionDays == 0 {
		m.RetentionDays = 15
	}
	if m.MetricsPort == 0 {
		m.MetricsPort = 9090
	}
	return m
}

// ResolvedPlacement returns the PVE node/storage for the monitoring node,
// falling back to the global Proxmox settings.
func (m MonitoringSpec) ResolvedPlacement(px Proxmox) (node, storage string) {
	node, storage = px.Node, px.Storage
	if m.Node != "" {
		node = m.Node
	}
	if m.Storage != "" {
		storage = m.Storage
	}
	return node, storage
}

// RegistryCacheSpec declares the container-registry pull-through cache that
// runs on the utility host (the VM declared by [monitoring]). It is a plain
// caching proxy in front of an upstream registry: runner nodes point their
// container runtime at it, so an image layer crosses the WAN once and is then
// served from the LAN for every subsequent job.
//
// The cache is deliberately bounded on disk. Its contents are 100% derivable
// from upstream, so when it exceeds MaxSizeGB the host drops the blob store
// and starts cold rather than filling the utility disk.
type RegistryCacheSpec struct {
	// Enabled turns the cache on. Absent/false leaves the utility host exactly
	// as it was before this feature existed.
	Enabled bool `toml:"enabled"`
	// Port is the LAN port the cache listens on (default 5000).
	Port int `toml:"port"`
	// Upstream is the registry being cached (default Docker Hub). One cache
	// instance proxies exactly one upstream — that is a registry:2 constraint,
	// not a mayfly one.
	Upstream string `toml:"upstream"`
	// MaxSizeGB caps the on-disk blob store (default 20). See the type comment
	// for what happens at the cap.
	MaxSizeGB int `toml:"max_size_gb"`
	// TTL expires cached blobs that have not been re-pulled (default "168h").
	// This is the gentle, continuous bound; MaxSizeGB is the hard backstop.
	TTL string `toml:"ttl"`
	// Username/Password optionally authenticate to the upstream. For Docker Hub
	// this lifts the anonymous pull-rate limit for everything behind the cache.
	Username string `toml:"username"`
	Password string `toml:"password"`
	// Endpoint overrides the mirror URL handed to runner nodes. Empty (the
	// normal case) derives it from the utility host's own LAN address once it
	// has been provisioned, so nothing about the fleet's addressing is baked
	// into mayfly. Set it when the host is reachable by a DNS name, sits behind
	// a load balancer, or terminates TLS elsewhere.
	Endpoint string `toml:"endpoint"`
	// FailOpen tells runner nodes to fall back to the upstream registry when
	// the cache is unreachable, rather than failing the pull. nil = true: a
	// cache outage must not take CI down with it.
	FailOpen *bool `toml:"fail_open"`

	// VM, when present, moves the cache OFF the [monitoring] utility host and
	// onto a VM of its own. Absent (the normal case) keeps it colocated with
	// Prometheus/Grafana, which is where it has always run.
	//
	// The reason this exists: runner isolation. Once the fleet's runners moved
	// onto their own VLAN, they could no longer route to the utility host's LAN
	// address, so every pull silently fell through to the upstream registry --
	// FailOpen is on by default, so nothing broke and nothing logged. A cache
	// the runners cannot reach is a cache with a 0% hit ratio.
	//
	// Colocating forced a bad choice: put the whole utility host on the runner
	// VLAN (losing LAN access to Grafana, and parking Grafana's admin
	// credentials on the VLAN that runs untrusted job code), or poke a hole
	// from the runner VLAN back to the LAN. Giving the cache its own VM avoids
	// both: it sits on the runner VLAN where the runners already are, holds
	// nothing but derivable image blobs, and leaves monitoring alone.
	VM *RegistryCacheVMSpec `toml:"vm"`

	// Actions, when enabled, runs a SECOND registry:2 on the dedicated cache
	// VM: a writable one, holding the GitHub Actions cache entries ephemerd's
	// [actions_cache] shim stores as OCI artifacts (ephemerd #215). It cannot
	// share the pull-through instance — a registry:2 with a proxy remote URL
	// refuses every push. Requires [registry_cache.vm].
	Actions *ActionsRegistrySpec `toml:"actions"`
}

// ActionsRegistrySpec is the writable registry behind ephemerd's LAN Actions
// cache. Unlike the pull-through cache it is authenticated: job containers can
// reach the cache VM, and only ephemerd — never a job — may write to it.
type ActionsRegistrySpec struct {
	Enabled bool `toml:"enabled"`
	// Port is the LAN port it listens on (default 5001; the pull-through cache
	// keeps 5000).
	Port int `toml:"port"`
	// MaxSizeGB is the hard cap on its blob store (default 150). ephemerd
	// evicts per repo long before this; the cap is the backstop that stops the
	// cache VM's disk filling if eviction ever falls behind.
	MaxSizeGB int `toml:"max_size_gb"`
	// Username is the htpasswd user ephemerd authenticates as (default
	// "ephemerd").
	Username string `toml:"username"`
	// PasswordEnv names an environment variable holding the password
	// (preferred — keeps it out of config.toml). Password is the inline
	// fallback. One of the two is required when enabled.
	PasswordEnv string `toml:"password_env"`
	Password    string `toml:"password"`
}

// Resolved fills zero fields with defaults.
func (a ActionsRegistrySpec) Resolved() ActionsRegistrySpec {
	if a.Port == 0 {
		a.Port = 5001
	}
	if a.MaxSizeGB == 0 {
		a.MaxSizeGB = 150
	}
	if a.Username == "" {
		a.Username = "ephemerd"
	}
	return a
}

// ResolvedPassword returns the password, preferring PasswordEnv over the
// inline Password, matching how github_extra resolves token_env.
func (a ActionsRegistrySpec) ResolvedPassword() string {
	if a.PasswordEnv != "" {
		if v := os.Getenv(a.PasswordEnv); v != "" {
			return v
		}
	}
	return a.Password
}

// ActionsActive reports whether the writable Actions registry should run: it
// exists only on a dedicated cache VM, so the colocated mode never gets one.
func (r RegistryCacheSpec) ActionsActive() bool {
	return r.Dedicated() && r.Actions != nil && r.Actions.Enabled
}

// ActionsEndpoint returns the Actions registry URL for runner nodes, derived
// from the cache VM's recorded address. "" when inactive or not yet known.
func (r RegistryCacheSpec) ActionsEndpoint(hostIP string) string {
	if !r.ActionsActive() || hostIP == "" {
		return ""
	}
	return fmt.Sprintf("http://%s:%d", hostIP, r.Actions.Resolved().Port)
}

// RegistryCacheVMSpec places the pull-through cache on a dedicated VM. Its
// fields mirror MonitoringSpec's -- same provisioning path, same defaults --
// plus VLANTag, which is the entire point of splitting the two apart.
type RegistryCacheVMSpec struct {
	// Node/Storage place the VM. Empty falls back to [proxmox] defaults, as
	// everywhere else.
	Node    string `toml:"node"`
	Storage string `toml:"storage"`
	// Template is the base image. Required: there is no sane default, and
	// guessing one would provision the wrong OS.
	Template string `toml:"template"`
	Cores    int    `toml:"cores"`     // default 2
	MemoryMB int    `toml:"memory_mb"` // default 2048
	DiskGB   int    `toml:"disk_gb"`   // default 40
	// VMStorage moves the VM's disk off the template's storage onto another
	// one (e.g. a large lvm-thin pool), as pools' vm_storage does. Empty keeps
	// the disk where the clone put it. On an existing VM a change is applied
	// LIVE with a Proxmox disk move — the VM, its address and its blobs all
	// survive — rather than by recreating it.
	VMStorage string `toml:"vm_storage"`
	// VLANTag tags the cache VM's vNIC (1-4094). 0 = untagged. Set this to the
	// runners' VLAN so they can actually reach the cache.
	VLANTag int `toml:"vlan_tag"`
	// Firewall enables the hypervisor's per-vNIC firewall, as on pool members.
	Firewall bool `toml:"firewall"`
}

// Resolved fills zero fields with defaults. The cache is a blob store and a
// reverse proxy: it needs disk, not much else.
func (v RegistryCacheVMSpec) Resolved() RegistryCacheVMSpec {
	if v.Cores == 0 {
		v.Cores = 2
	}
	if v.MemoryMB == 0 {
		v.MemoryMB = 2048
	}
	if v.DiskGB == 0 {
		v.DiskGB = 40
	}
	return v
}

// ResolvedPlacement resolves node/storage against the [proxmox] defaults.
func (v RegistryCacheVMSpec) ResolvedPlacement(px Proxmox) (node, storage string) {
	node, storage = px.Node, px.Storage
	if v.Node != "" {
		node = v.Node
	}
	if v.Storage != "" {
		storage = v.Storage
	}
	return node, storage
}

// Dedicated reports whether the cache runs on its own VM rather than on the
// utility host. This is the single predicate the rest of the code branches on.
func (r RegistryCacheSpec) Dedicated() bool {
	return r.Enabled && r.VM != nil
}

// OnMonitoring is the cache spec as the UTILITY HOST sees it: unchanged when
// colocated, and disabled once the cache has its own VM. Everything that
// renders or fingerprints the monitoring host goes through this, so moving the
// cache onto its own VM also removes it from the utility host in the same
// apply, instead of leaving a second copy running there.
func (r RegistryCacheSpec) OnMonitoring() RegistryCacheSpec {
	if r.Dedicated() {
		return RegistryCacheSpec{}
	}
	return r
}

// Resolved fills zero fields with defaults.
func (r RegistryCacheSpec) Resolved() RegistryCacheSpec {
	if r.Port == 0 {
		r.Port = 5000
	}
	if r.Upstream == "" {
		r.Upstream = "https://registry-1.docker.io"
	}
	if r.MaxSizeGB == 0 {
		r.MaxSizeGB = 20
	}
	if r.TTL == "" {
		r.TTL = "168h"
	}
	return r
}

// ResolvedFailOpen reports the fail-open setting, defaulting to true.
func (r RegistryCacheSpec) ResolvedFailOpen() bool {
	return r.FailOpen == nil || *r.FailOpen
}

// MirrorEndpoint returns the URL runner nodes should use for the cache. The
// explicit Endpoint wins; otherwise it is derived from hostIP, the utility
// host's LAN address as recorded in state. Returns "" when neither is
// available — the caller omits the mirror rather than guessing.
func (r RegistryCacheSpec) MirrorEndpoint(hostIP string) string {
	if !r.Enabled {
		return ""
	}
	if r.Endpoint != "" {
		return strings.TrimRight(r.Endpoint, "/")
	}
	if hostIP == "" {
		return ""
	}
	return fmt.Sprintf("http://%s:%d", hostIP, r.Resolved().Port)
}

// ModelServerSpec declares the fleet's LLM server: Ollama on a dedicated VM
// with a GPU passed through, serving the OpenAI-compatible API on Port.
//
// It is shaped like [registry_cache] with a [registry_cache.vm], minus the
// colocated mode: a model server is useless without its GPU, and a GPU
// belongs to exactly one VM, so the VM block is mandatory.
type ModelServerSpec struct {
	// Enabled turns the server on. Absent/false provisions nothing.
	Enabled bool `toml:"enabled"`
	// Port is where Ollama listens (default 11434, Ollama's own).
	Port int `toml:"port"`
	// Models are pulled on first boot, in the background so a large download
	// does not hold up provisioning (default ["qwen3:8b"]).
	Models []string `toml:"models"`
	// VM places the server. Required when enabled.
	VM *ModelServerVMSpec `toml:"vm"`
}

// ModelServerVMSpec is the model server's VM. Its fields mirror
// RegistryCacheVMSpec, plus HostPCI.
type ModelServerVMSpec struct {
	Node     string `toml:"node"`
	Storage  string `toml:"storage"`
	Template string `toml:"template"`
	Cores    int    `toml:"cores"`     // default 4
	MemoryMB int    `toml:"memory_mb"` // default 8192
	DiskGB   int    `toml:"disk_gb"`   // default 100
	VLANTag  int    `toml:"vlan_tag"`
	Firewall bool   `toml:"firewall"`
	// HostPCI lists the GPUs to pass through: each entry is either a PCI
	// address ("0000:07:00.0", "07:00.0") or the name of a Datacenter →
	// Resource Mappings → PCI entry. Use a mapping name when mayfly
	// authenticates with an API token: Proxmox only lets root@pam itself
	// pass a raw address through.
	//
	// Passthrough pins all of MemoryMB on the host for the VM's lifetime —
	// no ballooning, no overcommit — so size MemoryMB against what the
	// node really has free. The model lives in VRAM; system RAM is only a
	// staging area while it loads, which is why the default is modest.
	HostPCI []string `toml:"hostpci"`
}

// Resolved fills zero fields with defaults.
func (m ModelServerSpec) Resolved() ModelServerSpec {
	if m.Port == 0 {
		m.Port = 11434
	}
	if len(m.Models) == 0 {
		m.Models = []string{"qwen3:8b"}
	}
	return m
}

// Resolved fills zero fields with defaults.
func (v ModelServerVMSpec) Resolved() ModelServerVMSpec {
	if v.Cores == 0 {
		v.Cores = 4
	}
	if v.MemoryMB == 0 {
		v.MemoryMB = 8192
	}
	if v.DiskGB == 0 {
		v.DiskGB = 100
	}
	return v
}

// ResolvedPlacement resolves node/storage against the [proxmox] defaults.
func (v ModelServerVMSpec) ResolvedPlacement(px Proxmox) (node, storage string) {
	node, storage = px.Node, px.Storage
	if v.Node != "" {
		node = v.Node
	}
	if v.Storage != "" {
		storage = v.Storage
	}
	return node, storage
}

// Active reports whether a model server VM should exist.
func (m ModelServerSpec) Active() bool {
	return m.Enabled && m.VM != nil
}

// Endpoint is the OpenAI-compatible base URL for the server at hostIP, the
// address recorded in state once the VM is up, or "" when there is none
// yet. ephemerd's failure triage will be pointed at it as its `[llm]
// endpoint`.
func (m ModelServerSpec) Endpoint(hostIP string) string {
	if !m.Enabled || hostIP == "" {
		return ""
	}
	return fmt.Sprintf("http://%s:%d/v1", hostIP, m.Resolved().Port)
}

// IsPCIAddress reports whether s is a raw PCI address ("0000:07:00.0",
// "07:00.0", or "07:00" for every function) rather than a resource-mapping
// name.
func IsPCIAddress(s string) bool {
	return pciAddrRe.MatchString(s)
}

var (
	pciAddrRe        = regexp.MustCompile(`^([0-9a-fA-F]{4}:)?[0-9a-fA-F]{2}:[0-9a-fA-F]{2}(\.[0-7])?$`)
	pciMappingNameRe = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_-]*$`)
)

// MetricsEnabled reports whether ephemerd's /metrics endpoint should be turned
// on across the fleet (true whenever a monitoring node is configured).
func (c *Config) MetricsEnabled() bool { return c.Monitoring != nil }

// MetricsPort is the port ephemerd exposes /metrics on (0 when disabled).
func (c *Config) MetricsPort() int {
	if c.Monitoring == nil {
		return 0
	}
	return c.Monitoring.Resolved().MetricsPort
}

// MetricsBindAddr is the interface runner nodes bind /metrics to, or "" to
// leave ephemerd's loopback-only default in place. See
// MonitoringSpec.MetricsBindAddr for why an unset value means Prometheus
// scrapes nothing.
func (c *Config) MetricsBindAddr() string {
	if c.Monitoring == nil {
		return ""
	}
	return c.Monitoring.Resolved().MetricsBindAddr
}

type PoolSpec struct {
	Hostname string `toml:"hostname"`
	Size     int    `toml:"size"`
	OS       string `toml:"os"`   // "linux" (default) or "windows"
	Arch     string `toml:"arch"` // "amd64" (default) or "arm64"
	// Provider selects the hypervisor backend for this pool's VMs. The only
	// supported value is "proxmox" (also the default when empty). Ignored for
	// enrolled (SSH bare-metal) pools, which have no hypervisor.
	Provider string   `toml:"provider"`
	Cores    int      `toml:"cores"`
	MemoryMB int      `toml:"memory_mb"`
	DiskGB   int      `toml:"disk_gb"`
	Repos    []string `toml:"repos"`
	// MaxConcurrent caps parallel jobs per member (ephemerd's
	// runner.max_concurrent; 0 = ephemerd's default of 4). Config-tier:
	// changes apply with a restart, no rebuild.
	MaxConcurrent int `toml:"max_concurrent"`
	// JobTimeout overrides ephemerd's [runner] job_timeout — the maximum wall
	// time one job may run before it's killed (Go duration, e.g. "2h"). Empty =
	// ephemerd's default (2h). Config-tier.
	JobTimeout string `toml:"job_timeout"`
	// OrphanGrace overrides ephemerd's [runner.orphan_sweep] grace — how long a
	// dispatched runner may sit unassigned before it's destroyed (Go duration,
	// e.g. "30m"). Empty = ephemerd's default (10m). Raise it for slow-booting
	// pools (windows, arm64 VMs) where the 10m default orphans a runner before
	// GitHub hands it a job. Config-tier.
	OrphanGrace string `toml:"orphan_grace"`
	// DrainTimeout bounds how long apply waits for a node's running jobs to
	// finish before restarting ephemerd anyway (Go duration, e.g. "45m").
	// During the wait the pool's webhooks are deactivated (cordon), so no new
	// jobs arrive; at the deadline apply proceeds with a loud warning rather
	// than hanging forever behind back-to-back jobs. Empty = 45m. "0" =
	// restart immediately (the old, job-killing behavior).
	DrainTimeout string `toml:"drain_timeout"`
	// EphemerdVersion pins this pool to a specific ephemerd release,
	// overriding the global [ephemerd] version — canary a new release on
	// one pool before rolling the fleet. Config-tier: apply pushes the new
	// binary + restarts.
	EphemerdVersion string `toml:"ephemerd_version"`

	// GitHubExtra adds secondary GitHub targets this pool's ephemerd serves
	// alongside the primary [github] owner (requires ephemerd >= v0.1.3). Use
	// to lend a pool to another org/user's CI — e.g. serve luthermonson/* from
	// the same box that runs the ephpm org's jobs. Config-tier.
	GitHubExtra []GitHubTarget `toml:"github_extra"`

	// DispatchPolicy narrows the fleet-wide [github] dispatch_policy for this
	// pool's PRIMARY target only. Merged per-field over the global one (a set
	// field wins, an empty field inherits), so a pool can only tighten dispatch,
	// never loosen it. Extra targets are NOT covered by this — they are separate
	// GitHubConfig blocks in ephemerd with their own policy; see
	// EffectiveDispatchPolicyFor. Config-tier.
	DispatchPolicy DispatchPolicy `toml:"dispatch_policy"`

	// ActionsCache opts this pool's nodes into ephemerd's LAN Actions cache
	// ([actions_cache], ephemerd #215), backed by the writable registry that
	// [registry_cache.actions] runs on the cache VM. Per pool so it can be
	// turned on for one node before the fleet. Linux pools only: ephemerd's
	// feature is Linux-only, so validation refuses it elsewhere. Config-tier.
	ActionsCache bool `toml:"actions_cache"`

	// VM sizes ephemerd's per-job guest VMs on darwin hosts — the macOS VM and
	// the linux-arm64 sidecar VM ([vm.macos] / [vm.linux]). Only meaningful for
	// darwin pools, where a fixed-RAM Mac mini must fit host + macOS VM + linux
	// sidecar; shrink them to keep an 8 GB box off the OOM killer. Zero fields =
	// ephemerd's defaults (macOS 8 GB, linux 2 GB). Config-tier.
	VM VMSizing `toml:"vm"`

	// WindowsRunner sizes the Hyper-V-isolated container each Windows job runs
	// in (ephemerd [runner.windows]). Only meaningful for windows pools. Zero =
	// ephemerd's default of 4 GB / 2 CPU per job container — too small for
	// heavy MSVC / fat-LTO builds, which OOM inside the 4 GB box while the VM's
	// remaining RAM sits idle. Config-tier. See [[pools]] docs.
	WindowsRunner WindowsRunnerSpec `toml:"windows_runner"`

	// Template overrides the base VM template for this pool. Required for
	// windows (there is no downloadable Windows cloud image — the template
	// is built once by hand/Packer). Defaults for linux to
	// "mayfly-ubuntu-24.04-<arch>", which apply creates automatically.
	Template string `toml:"template"`
	// Node/Storage place this pool on a specific PVE node/storage (e.g. an
	// arm64 host). Default to [proxmox] node/storage.
	Node    string `toml:"node"`
	Storage string `toml:"storage"`
	// VMStorage puts this pool's runner VM disk on a storage separate from the
	// template's, via a FULL clone. Empty (the default) does a LINKED clone,
	// which forces the disk onto the template's storage — byte-for-byte the
	// historical behavior, so an existing pool is unaffected. Set it when the
	// template must sit on a small import-capable dir store (e.g. "local") but
	// the VM disk needs a bigger/separate store (e.g. a "local-lvm" thinpool):
	// this is the fix for the node-b node, whose only big storage is a 794 GiB
	// block thinpool while templates need "local" (~85 GiB), which repeatedly
	// filled the disk. A full clone COPIES the disk (slower provision, uses real
	// space on VMStorage) where a linked clone only references the template's —
	// the intended tradeoff. This is deliberately per-pool ONLY: there is no
	// global [proxmox] vm_storage fallback, because a fleet-wide default would
	// silently turn every pool into a full clone. Must accept disk images
	// ('images'/'rootdir' content) on the PVE host. Setting vm_storage equal to
	// the pool's disk storage is legal but pointless — it does a same-storage
	// full copy, spending the time and space of a copy for the placement a
	// linked clone already gives; leave it empty in that case.
	VMStorage string `toml:"vm_storage"`
	// VLANTag puts this pool's runner vNICs on a tagged VLAN (Proxmox net0
	// "tag=<n>"), which is how a fleet isolates runners from the flat LAN:
	// untrusted CI workloads land on their own layer-2 segment and the router
	// decides what they may reach. Valid range 1–4094; 0/unset leaves the NIC
	// untagged (the historical behavior — an existing config produces exactly
	// the VM network config it did before). Requires the pool's bridge to be
	// VLAN-aware on the PVE host ("bridge-vlan-aware yes" on vmbr0) and the
	// VLAN to exist on the upstream switch/router. Rebuild-tier — it is a NIC
	// property written at clone time, so a changed tag forces the affected
	// members to be replaced (the planner compares state.Member.VLANTag against
	// this spec); existing members keep their old tag until that replace runs.
	VLANTag int `toml:"vlan_tag"`
	// Firewall enables the Proxmox firewall on this pool's runner vNICs
	// (net0 "firewall=1"). Default false = the historical behavior (no
	// per-NIC firewall). Turning it on is only half the job: the firewall
	// must also be enabled at the datacenter and VM level, and the default
	// policy plus rules live outside mayfly — see the note in
	// internal/pve.BuildNet0 and the loud plan-time warning mayfly emits while
	// this is set. Rebuild-tier, same as vlan_tag.
	Firewall bool `toml:"firewall"`
	// AllowLAN is the deliberate opt-out of the isolation-required guard: a
	// pool that provisions Proxmox VMs must set a vlan_tag (isolating its
	// runners on their own layer-2 segment) OR set allow_lan = true to accept
	// that its runners join the trusted flat LAN. Without one of the two,
	// validation fails — this is what stops a new VM pool from silently landing
	// untrusted CI on the LAN just by forgetting the tag. Meaningless (and
	// rejected) for enrolled bare-metal and darwin pools, which have no vNIC
	// mayfly controls and are isolated by other means.
	AllowLAN bool `toml:"allow_lan"`

	// CloudInitStorage overrides [proxmox] cloud_init_storage for this pool —
	// the storage the pool's per-VM cloud-init ISO lands on (must accept 'iso'
	// content). Empty = fall back to the global cloud_init_storage, then to
	// the pool's disk storage (historical behavior).
	CloudInitStorage string `toml:"cloud_init_storage"`

	// Members enumerates bare-metal hosts enrolled over SSH: every os =
	// "darwin" pool (Mac minis), or an os = "linux" pool that names existing
	// linux boxes (e.g. an arm64 server) instead of provisioning PVE VMs.
	// Enrolled members have no size/template/clone lifecycle — mayfly cannot
	// create or destroy the hardware; apply just converges ephemerd
	// config/binary + service on each host. A linux pool WITHOUT members keeps
	// the ordinary PVE VM lifecycle. Not valid for windows.
	Members []string `toml:"members"`
	// SSHUser is the account used to enroll members. Must have passwordless
	// sudo. By default the system ssh binary is used (agent, ~/.ssh
	// identities, ssh_config incl. ProxyJump all apply).
	SSHUser string `toml:"ssh_user"`
	// SSHKey pins a specific private key (system ssh -i + IdentitiesOnly).
	SSHKey string `toml:"ssh_key"`
	// SSHPort overrides port 22.
	SSHPort int `toml:"ssh_port"`
	// SSHPassword switches to password auth via an embedded SSH client
	// (system ssh cannot take a password non-interactively). Bypasses
	// ssh_config and host-key verification — prefer keys.
	SSHPassword string `toml:"ssh_password"`
}

// Resolved returns the spec with Defaults applied for zero fields.
func (p PoolSpec) Resolved(d Defaults) PoolSpec {
	if p.OS == "" {
		p.OS = "linux"
	}
	if p.Provider == "" {
		p.Provider = ProviderProxmox
	}
	if p.Arch == "" {
		if p.OS == "darwin" {
			p.Arch = "arm64" // M-series; the only darwin release asset
		} else {
			p.Arch = "amd64"
		}
	}
	if p.UsesEnroll() {
		// Enrolled (SSH bare-metal) pools take their size from the member
		// list — mayfly can't create/destroy the hardware. Darwin is always
		// enrolled; a linux pool is enrolled only when members are declared.
		p.Size = len(p.Members)
		if p.SSHUser == "" {
			p.SSHUser = "root"
		}
	}
	if p.Cores == 0 {
		p.Cores = d.Cores
	}
	if p.MemoryMB == 0 {
		p.MemoryMB = d.MemoryMB
	}
	if p.DiskGB == 0 {
		p.DiskGB = d.DiskGB
	}
	// A linux pool that enrolls bare metal never provisions a PVE VM, so it
	// needs no base template.
	if p.Template == "" && p.OS == "linux" && !p.UsesEnroll() {
		p.Template = "mayfly-ubuntu-24.04-" + p.Arch
	}
	return p
}

// ResolvedDispatchPolicy returns the effective policy for this pool's PRIMARY
// [github] target: the pool's own block merged per-field over the fleet-wide
// one.
func (p PoolSpec) ResolvedDispatchPolicy(g GitHub) DispatchPolicy {
	return p.DispatchPolicy.mergeOver(g.DispatchPolicy)
}

// EffectiveDispatchPolicyFor returns the policy to render for one of this
// pool's extra targets.
//
// ephemerd applies each target's DispatchPolicy independently — cmd/ephemerd
// builds one provider per entry of GitHubTargets() and reads that entry's own
// block — so an extra target rendered without a policy is fully ungated no
// matter what [github.dispatch_policy] says. That is the same class of silent
// gap as mayfly#23 (extra targets missing default_image_*), and here it is a
// security control rather than an image default, so mayfly fills it in:
//
//   - required_labels: inherited from the pool's effective policy when the
//     target sets none. Labels are a property of the workflow, not of the owner,
//     so inheriting is meaningful and can only tighten dispatch.
//   - allowed_repos: NEVER inherited. ephemerd matches bare repo names against
//     the webhook's repository.name, and those names are scoped to the target's
//     owner. Copying the primary owner's repo list onto another owner's target
//     would match nothing and black-hole every job it serves. A target that
//     wants a repo allowlist must state its own.
func (p PoolSpec) EffectiveDispatchPolicyFor(t GitHubTarget, g GitHub) DispatchPolicy {
	out := t.DispatchPolicy
	if len(out.RequiredLabels) == 0 {
		out.RequiredLabels = p.ResolvedDispatchPolicy(g).RequiredLabels
	}
	return out
}

// UsesEnroll reports whether this pool is provisioned by SSH bare-metal
// enrollment rather than the PVE VM lifecycle. Darwin pools are always
// enrolled (mayfly cannot create Mac minis); a linux pool is enrolled when it
// declares members (existing bare-metal hosts). Windows never enrolls.
//
// Callers must guard against a windows pool with a stray members list (barred
// by validation): UsesEnroll returns false for windows regardless of members.
func (p PoolSpec) UsesEnroll() bool {
	switch p.OS {
	case "darwin":
		return true
	case "linux", "":
		return len(p.Members) > 0
	default:
		return false
	}
}

// ResolvedProvider returns the hypervisor backend this pool's VMs run on:
// its explicit provider field, or the default (Proxmox) when unset. Safe to
// call on an unresolved spec. Meaningless for enrolled (SSH bare-metal) pools,
// which have no hypervisor — callers guard with UsesEnroll.
func (p PoolSpec) ResolvedProvider() Provider {
	if p.Provider == "" {
		return ProviderProxmox
	}
	return Provider(p.Provider)
}

// ResolvedPlacement returns the PVE node and storage for this (already
// Resolved) spec, falling back to the global Proxmox settings. A per-pool
// node/storage override wins.
func (p PoolSpec) ResolvedPlacement(px Proxmox) (node, storage string) {
	node, storage = px.Node, px.Storage
	if p.Node != "" {
		node = p.Node
	}
	if p.Storage != "" {
		storage = p.Storage
	}
	return node, storage
}

// ResolvedCloudInitStorage returns the storage the pool's cloud-init ISO
// should land on: the per-pool cloud_init_storage override, else the global
// [proxmox] cloud_init_storage, else "" — where "" means "put the ISO on the
// same storage as the VM disk" (the historical, behavior-preserving default).
func (p PoolSpec) ResolvedCloudInitStorage(px Proxmox) string {
	if p.CloudInitStorage != "" {
		return p.CloudInitStorage
	}
	return px.CloudInitStorage
}

// VMSizing overrides the sizing of ephemerd's per-job guest VMs on a darwin
// host: the macOS VM that runs macOS jobs and the linux-arm64 sidecar VM that
// runs linux jobs. Rendered as [vm.macos] / [vm.linux] in the daemon config.
type VMSizing struct {
	MacOS VMSpec `toml:"macos"`
	Linux VMSpec `toml:"linux"`
}

// VMSpec is per-VM resource sizing. Zero fields fall back to ephemerd's
// built-in defaults for that VM type (macOS: 4 cpu / 8 GB; linux: 2 cpu /
// 2 GB / 50 GB disk).
type VMSpec struct {
	// Enabled toggles whether the host serves jobs through this VM at all
	// (ephemerd [vm.linux] enabled; requires ephemerd > v0.1.4 to take
	// effect, older daemons parse and ignore it). Pointer: omitted keeps
	// ephemerd's platform default (darwin serves linux jobs, windows opt-in).
	// Set `enabled = false` on a darwin pool's [vm.linux] when a dedicated
	// linux box serves the same labels — dual-claiming orphans a runner per
	// job. Only meaningful for [vm.linux] today.
	Enabled    *bool `toml:"enabled"`
	CPUs       int   `toml:"cpus"`
	MemoryMB   int   `toml:"memory_mb"`
	DiskSizeGB int   `toml:"disk_size_gb"`
}

// IsZero reports whether any VM sizing override is set.
func (v VMSizing) IsZero() bool {
	return v.MacOS == VMSpec{} && v.Linux == VMSpec{}
}

// WindowsRunnerSpec sizes the Hyper-V-isolated container each Windows job runs
// in, rendered as [runner.windows] in the daemon config. Zero fields fall back
// to ephemerd's defaults (4096 MB / 2 CPU).
type WindowsRunnerSpec struct {
	MemoryMB int `toml:"memory_mb"`
	CPUs     int `toml:"cpus"`
}

// IsZero reports whether any Windows runner override is set.
func (w WindowsRunnerSpec) IsZero() bool { return w == WindowsRunnerSpec{} }

type Proxmox struct {
	URL     string `toml:"url"`
	TokenID string `toml:"token_id"`
	Secret  string `toml:"secret"`
	Node    string `toml:"node"`
	Storage string `toml:"storage"`
	// CloudInitStorage places the generated per-VM cloud-init ISO on a
	// storage that accepts 'iso' content (e.g. a dir store like "local"),
	// separate from Storage (the VM's OS disk). Required when Storage is a
	// block store (ZFS zpool, LVM-thin) that only accepts 'images'/'rootdir'
	// content. Empty = put the ISO on the same storage as the disk (the
	// historical behavior, fine for dir stores).
	CloudInitStorage string `toml:"cloud_init_storage"`
	Insecure         bool   `toml:"insecure_skip_verify"`
}

type Cloudflare struct {
	APIToken   string `toml:"api_token"`
	AccountID  string `toml:"account_id"`
	ZoneID     string `toml:"zone_id"`
	BaseDomain string `toml:"base_domain"`
}

type GitHub struct {
	Owner          string `toml:"owner"`
	AppID          int64  `toml:"app_id"`
	InstallationID int64  `toml:"installation_id"`
	PrivateKeyPath string `toml:"private_key_path"`
	// DefaultImageLinux/Windows override the runner container image ephemerd
	// launches for Linux/Windows jobs across the fleet. Empty = ephemerd's
	// built-in defaults (actions-runner for Linux, host-matched servercore for
	// Windows). Set to the CI images (e.g. ephpm/ephemerd:runner-ci-linux) that
	// ship the Go/mage build toolchain so jobs don't reinstall it every run.
	DefaultImageLinux   string `toml:"default_image_linux"`
	DefaultImageWindows string `toml:"default_image_windows"`
	// DispatchPolicy is the fleet-wide default dispatch allowlist rendered into
	// every node's [github.dispatch_policy]. Zero = no restriction (ephemerd's
	// historical behavior). A pool may narrow it per-field via
	// [pools.<name>.dispatch_policy]; see PoolSpec.DispatchPolicy.
	DispatchPolicy DispatchPolicy `toml:"dispatch_policy"`
}

// DispatchPolicy mirrors ephemerd's [github.dispatch_policy] — an OPT-IN
// allowlist gating which incoming workflow_job webhook events may dispatch a
// runner. It is defence in depth UNDER GitHub's "require approval for outside
// collaborators" setting, not a replacement for it: the approval gate is what
// actually stops a fork PR from running on your hardware.
//
// Both fields default to "no restriction". Setting either narrows dispatch;
// ephemerd ANDs them (allowed repo AND at least one required label).
type DispatchPolicy struct {
	// AllowedRepos, when non-empty, restricts dispatch to jobs whose repository
	// is listed. Empty = every repo the target already tracks.
	//
	// These are BARE repo names, not "owner/repo": ephemerd matches them against
	// the webhook's repository.name, the same value used by `repos`. A slash
	// here matches nothing and black-holes the target, so Validate rejects it.
	// The names are also owner-scoped, which is why they are never inherited
	// across targets — see EffectiveDispatchPolicyFor.
	AllowedRepos []string `toml:"allowed_repos"`
	// RequiredLabels, when non-empty, requires an incoming job to carry at least
	// one of these labels beyond the mandatory "self-hosted" gate. Empty = no
	// extra label requirement. This is the useful knob for a public repo: pin
	// dispatch to a distinctive label your own workflows opt into and an
	// outside fork would have to guess (e.g. "ephemerd").
	RequiredLabels []string `toml:"required_labels"`
}

// IsZero reports whether the policy imposes no restriction at all — the
// unset default. Mirrors ephemerd's DispatchPolicyConfig.IsZero.
func (d DispatchPolicy) IsZero() bool {
	return len(d.AllowedRepos) == 0 && len(d.RequiredLabels) == 0
}

// mergeOver returns d with each EMPTY field filled from base. Per-field rather
// than whole-block, deliberately: a pool that overrides only allowed_repos must
// not silently drop the fleet-wide required_labels. Inheritance here can only
// narrow dispatch or leave it unchanged, never loosen it.
func (d DispatchPolicy) mergeOver(base DispatchPolicy) DispatchPolicy {
	if len(d.AllowedRepos) == 0 {
		d.AllowedRepos = base.AllowedRepos
	}
	if len(d.RequiredLabels) == 0 {
		d.RequiredLabels = base.RequiredLabels
	}
	return d
}

// validate reports malformed entries. An empty or "owner/repo"-shaped entry is
// dead config that silently blocks dispatch, so it is an error rather than a
// warning — this control fails closed.
func (d DispatchPolicy) validate(where string) []error {
	var errs []error
	for i, r := range d.AllowedRepos {
		switch {
		case strings.TrimSpace(r) == "":
			errs = append(errs, fmt.Errorf("%s: dispatch_policy.allowed_repos[%d] is empty", where, i))
		case strings.Contains(r, "/"):
			errs = append(errs, fmt.Errorf(
				"%s: dispatch_policy.allowed_repos[%d] = %q must be a bare repo name, not owner/repo — ephemerd matches the webhook's repository name, so a slash matches nothing and blocks every job for this target",
				where, i, r))
		}
	}
	for i, l := range d.RequiredLabels {
		if strings.TrimSpace(l) == "" {
			errs = append(errs, fmt.Errorf("%s: dispatch_policy.required_labels[%d] is empty", where, i))
		}
	}
	return errs
}

// Dind configures ephemerd's in-daemon Docker shim (embedded BuildKit +
// containerd) so jobs can `docker build`/`run` without a real Docker daemon.
type Dind struct {
	Enabled bool `toml:"enabled"`
	// AllowPrivileged permits privileged job containers (some build tooling
	// needs it). Opt-in; ephemerd defaults it off on every platform.
	AllowPrivileged bool `toml:"allow_privileged"`
}

// ModuleProxy turns on ephemerd's per-runner Go module cache: a local GOPROXY
// on the bridge gateway, auto-injected into job containers as GOPROXY.
type ModuleProxy struct {
	Enabled bool `toml:"enabled"`
}

// CacheToggle is a fleet-wide on/off switch for an ephemerd package-cache proxy
// whose port/upstream tuning lives in ephemerd's own defaults (cargo crates,
// composer/Packagist, npm, PyPI, Dart pub, GitHub release assets). mayfly only
// decides whether it runs.
type CacheToggle struct {
	Enabled bool `toml:"enabled"`
}

// GitHubTarget is a secondary GitHub owner served by a pool's ephemerd daemon
// alongside the primary [github] owner (one daemon, many owners; requires
// ephemerd >= v0.1.3). Authenticated by a PAT: TokenEnv (preferred — names an
// environment variable holding the PAT, keeping the secret out of config.toml)
// takes precedence over an inline Token.
type GitHubTarget struct {
	Owner    string   `toml:"owner"`
	Repos    []string `toml:"repos"`
	Token    string   `toml:"token"`
	TokenEnv string   `toml:"token_env"`
	// DispatchPolicy gates this target's own webhook dispatch. ephemerd builds a
	// SEPARATE provider per target and applies each target's own policy, with no
	// inheritance from [github] — so a target left unset here is completely
	// ungated even when the fleet-wide policy is set. See
	// EffectiveDispatchPolicyFor for the partial inheritance mayfly applies to
	// close that gap.
	DispatchPolicy DispatchPolicy `toml:"dispatch_policy"`
}

// ResolvedToken returns the PAT, preferring TokenEnv over the inline Token.
func (t GitHubTarget) ResolvedToken() string {
	if t.TokenEnv != "" {
		if v := os.Getenv(t.TokenEnv); v != "" {
			return v
		}
	}
	return t.Token
}

type Ephemerd struct {
	Version    string `toml:"version"`
	BinaryURL  string `toml:"binary_url"`
	ConfigTOML string `toml:"config_toml"`
}

type Defaults struct {
	Cores    int `toml:"cores"`
	MemoryMB int `toml:"memory_mb"`
	DiskGB   int `toml:"disk_gb"`
}

func Path() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".mayfly", "config.toml"), nil
}

// loadOptions tunes how Load reads and validates the config.
type loadOptions struct {
	// skipCredentials drops the GitHub App and pool-token credential checks.
	// A single-node command (mayfly node status/exec/upgrade/drain) talks to
	// one node and authenticates to nothing, so it must not be blocked by
	// missing credentials for pools it never touches. Structure (proxmox
	// reachability, pool/member shape, transport) is still validated.
	skipCredentials bool
}

// Load reads, defaults, and fully validates the config — including every
// pool's GitHub App and token credentials. Used by plan/apply and anything
// that actually dispatches to GitHub.
func Load() (*Config, error) {
	return load(loadOptions{})
}

// LoadForNode is the scoped loader for single-node commands. It loads,
// applies env overrides, and validates the config's structure, but SKIPS the
// GitHub App / pool-token credential checks a one-node operation never uses.
// This lets `mayfly node status/exec/upgrade/drain` run without exporting
// every pool's GITHUB_TOKEN / EPHPM_* credentials.
func LoadForNode() (*Config, error) {
	return load(loadOptions{skipCredentials: true})
}

func load(opts loadOptions) (*Config, error) {
	p, err := Path()
	if err != nil {
		return nil, err
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", p, err)
	}
	var c Config
	if _, err := toml.Decode(string(b), &c); err != nil {
		return nil, fmt.Errorf("parse %s: %w", p, err)
	}
	c.applyEnvOverrides()
	c.applyDefaults()
	if err := c.validate(opts); err != nil {
		return nil, err
	}
	return &c, nil
}

func (c *Config) applyEnvOverrides() {
	if v := os.Getenv("EPHPM_APP_ID"); v != "" && c.GitHub.AppID == 0 {
		var id int64
		_, _ = fmt.Sscan(v, &id)
		c.GitHub.AppID = id
	}
	if v := os.Getenv("EPHPM_INSTALLATION_ID"); v != "" && c.GitHub.InstallationID == 0 {
		var id int64
		_, _ = fmt.Sscan(v, &id)
		c.GitHub.InstallationID = id
	}
	if v := os.Getenv("EPHPM_PRIVATE_KEY_PATH"); v != "" && c.GitHub.PrivateKeyPath == "" {
		c.GitHub.PrivateKeyPath = v
	}
}

func (c *Config) applyDefaults() {
	if c.Defaults.Cores == 0 {
		c.Defaults.Cores = 4
	}
	if c.Defaults.MemoryMB == 0 {
		c.Defaults.MemoryMB = 8192
	}
	if c.Defaults.DiskGB == 0 {
		c.Defaults.DiskGB = 60
	}
}

// Validate runs the full validation, including credentials. It is the
// exported entry point kept for callers (and tests) that want everything
// checked; Load / LoadForNode go through validate directly.
func (c *Config) Validate() error {
	return c.validate(loadOptions{})
}

func (c *Config) validate(opts loadOptions) error {
	var errs []error
	req := func(name, val string) {
		if val == "" {
			errs = append(errs, fmt.Errorf("missing %s", name))
		}
	}
	// Proxmox is only needed when a pool actually provisions VMs — a fleet
	// of nothing but SSH-enrolled darwin bare metal never touches PVE.
	if c.needsProxmox() {
		req("proxmox.url", c.Proxmox.URL)
		req("proxmox.token_id", c.Proxmox.TokenID)
		req("proxmox.secret", c.Proxmox.Secret)
		req("proxmox.node", c.Proxmox.Node)
		req("proxmox.storage", c.Proxmox.Storage)
	}
	// Cloudflare: either full API credentials, or nothing — the cloudflared
	// CLI + origin cert (~/.cloudflared/cert.pem from `cloudflared tunnel
	// login`) is the fallback, validated when the engine constructs.
	if c.Cloudflare.APIToken != "" {
		req("cloudflare.account_id", c.Cloudflare.AccountID)
		req("cloudflare.zone_id", c.Cloudflare.ZoneID)
	}
	// GitHub App / owner credentials authenticate runner registration and
	// job dispatch. A single-node command does none of that, so
	// skipCredentials drops these checks (see LoadForNode). Structure is
	// still validated below.
	if !opts.skipCredentials {
		req("github.owner", c.GitHub.Owner)
		if c.GitHub.AppID == 0 {
			errs = append(errs, errors.New("missing github.app_id (or EPHPM_APP_ID)"))
		}
		if c.GitHub.InstallationID == 0 {
			errs = append(errs, errors.New("missing github.installation_id (or EPHPM_INSTALLATION_ID)"))
		}
		if c.GitHub.PrivateKeyPath == "" {
			errs = append(errs, errors.New("missing github.private_key_path (or EPHPM_PRIVATE_KEY_PATH)"))
		}
	}
	errs = append(errs, c.GitHub.DispatchPolicy.validate("github")...)
	errs = append(errs, c.validatePools(opts)...)
	errs = append(errs, c.validateMonitoring()...)
	errs = append(errs, c.validateRegistryCache()...)
	errs = append(errs, c.validateModelServer()...)
	return errors.Join(errs...)
}

// validateModelServer checks the [model_server] block. Nothing is checked
// while it is disabled, so a half-written block can sit in the config.
func (c *Config) validateModelServer() []error {
	m := c.ModelServer
	if !m.Enabled {
		return nil
	}
	var errs []error
	if m.Port < 0 || m.Port > 65535 {
		errs = append(errs, fmt.Errorf("model_server: port %d out of range", m.Port))
	}
	for _, name := range m.Models {
		if strings.TrimSpace(name) == "" || strings.ContainsAny(name, " \t\r\n'\"`$;&|") {
			errs = append(errs, fmt.Errorf("model_server: model %q is not a valid model name (e.g. \"qwen3:8b\")", name))
		}
	}
	if m.VM == nil {
		return append(errs, errors.New(
			"model_server: enabled but no [model_server.vm] — the server needs a VM of its own to hold its GPU"))
	}
	v := *m.VM
	if strings.TrimSpace(v.Template) == "" {
		errs = append(errs, errors.New(
			"model_server.vm: template is required — there is no safe default base image to guess"))
	}
	if len(v.HostPCI) == 0 {
		errs = append(errs, errors.New(
			"model_server.vm: hostpci is empty — a model server without a GPU passed through would run every model on CPU; list the GPU's PCI address or resource-mapping name"))
	}
	for _, d := range v.HostPCI {
		if !IsPCIAddress(d) && !pciMappingNameRe.MatchString(d) {
			errs = append(errs, fmt.Errorf(
				"model_server.vm: hostpci %q is neither a PCI address (\"0000:07:00.0\") nor a resource-mapping name", d))
		}
	}
	if v.VLANTag < 0 || v.VLANTag > 4094 {
		errs = append(errs, fmt.Errorf("model_server.vm: vlan_tag %d out of range (1-4094, or 0 for untagged)", v.VLANTag))
	}
	if v.Cores < 0 {
		errs = append(errs, errors.New("model_server.vm: cores must be >= 0"))
	}
	if v.MemoryMB < 0 {
		errs = append(errs, errors.New("model_server.vm: memory_mb must be >= 0"))
	}
	if v.DiskGB < 0 {
		errs = append(errs, errors.New("model_server.vm: disk_gb must be >= 0"))
	}
	return errs
}

// validateRegistryCache checks the pull-through cache block. Everything here
// is skipped when the cache is off, so an existing config is unaffected.
func (c *Config) validateRegistryCache() []error {
	if !c.RegistryCache.Enabled {
		return nil
	}
	var errs []error
	r := c.RegistryCache
	// Colocated on the utility host, the cache needs [monitoring] to declare
	// that host. With its own [registry_cache.vm] it stands alone, so the
	// requirement lifts -- that is the point of splitting them.
	if c.Monitoring == nil && !r.Dedicated() {
		errs = append(errs, errors.New(
			"registry_cache: enabled but no [monitoring] section — the cache runs on the utility host that [monitoring] declares, unless you give it a [registry_cache.vm] of its own"))
	}
	if r.Port < 0 || r.Port > 65535 {
		errs = append(errs, fmt.Errorf("registry_cache: port %d out of range", r.Port))
	}
	if r.MaxSizeGB < 0 {
		errs = append(errs, errors.New("registry_cache: max_size_gb must be >= 0"))
	}
	res := r.Resolved()
	if err := validateAbsURL("registry_cache.upstream", res.Upstream); err != nil {
		errs = append(errs, err)
	}
	if r.Endpoint != "" {
		if err := validateAbsURL("registry_cache.endpoint", r.Endpoint); err != nil {
			errs = append(errs, err)
		}
	}
	if _, err := time.ParseDuration(res.TTL); err != nil {
		errs = append(errs, fmt.Errorf("registry_cache: ttl %q is not a duration (e.g. \"168h\")", res.TTL))
	}
	// A cap at or above the host disk cannot bound anything — the disk fills
	// before the cap trips. This fleet has lost nodes to full disks; refuse it.
	// Which disk to measure against depends on where the cache actually runs.
	switch {
	case r.Dedicated():
		// With the Actions registry on, both blob stores share the one disk,
		// so both caps count against it.
		capGB, what := res.MaxSizeGB, "max_size_gb"
		if r.ActionsActive() {
			capGB += r.Actions.Resolved().MaxSizeGB
			what = "max_size_gb + actions.max_size_gb"
		}
		if disk := r.VM.Resolved().DiskGB; capGB+registryCacheOSHeadroomGB > disk {
			errs = append(errs, fmt.Errorf(
				"registry_cache: %s (%d) plus %dGB for the OS must fit the cache VM's disk_gb (%d)",
				what, capGB, registryCacheOSHeadroomGB, disk))
		}
	case c.Monitoring != nil:
		if disk := c.Monitoring.Resolved().DiskGB; res.MaxSizeGB >= disk {
			errs = append(errs, fmt.Errorf(
				"registry_cache: max_size_gb (%d) must be smaller than the utility host's disk_gb (%d) — leave room for the OS and Prometheus",
				res.MaxSizeGB, disk))
		}
	}
	if r.VM != nil {
		errs = append(errs, validateRegistryCacheVM(*r.VM)...)
	}
	if r.Actions != nil && r.Actions.Enabled {
		errs = append(errs, validateActionsRegistry(r)...)
	}
	return errs
}

// registryCacheOSHeadroomGB is the disk a dedicated cache VM keeps free of
// blob stores for the OS, Docker's images and logs.
const registryCacheOSHeadroomGB = 10

// validateActionsRegistry checks [registry_cache.actions]. Only reached when it
// is enabled.
func validateActionsRegistry(r RegistryCacheSpec) []error {
	var errs []error
	if !r.Dedicated() {
		errs = append(errs, errors.New(
			"registry_cache.actions: needs a [registry_cache.vm] — the Actions registry runs only on the dedicated cache VM, on the runners' VLAN"))
	}
	a := r.Actions.Resolved()
	if a.Port < 1 || a.Port > 65535 {
		errs = append(errs, fmt.Errorf("registry_cache.actions: port %d out of range", a.Port))
	}
	if a.Port == r.Resolved().Port {
		errs = append(errs, fmt.Errorf("registry_cache.actions: port %d is the pull-through cache's port", a.Port))
	}
	if a.MaxSizeGB < 0 {
		errs = append(errs, errors.New("registry_cache.actions: max_size_gb must be >= 0"))
	}
	if a.ResolvedPassword() == "" {
		if a.PasswordEnv != "" {
			errs = append(errs, fmt.Errorf(
				"registry_cache.actions: password_env %s is empty — export it before running mayfly", a.PasswordEnv))
		} else {
			errs = append(errs, errors.New(
				"registry_cache.actions: set password_env (preferred) or password — the registry must not accept anonymous writes"))
		}
	}
	// htpasswd's line format and ephemerd's basic auth both break on these.
	if strings.ContainsAny(a.Username, ":\n\r ") {
		errs = append(errs, fmt.Errorf("registry_cache.actions: username %q must not contain ':' or whitespace", a.Username))
	}
	return errs
}

// validateRegistryCacheVM checks the dedicated-VM block. Only reached when the
// block is present, so a colocated cache is unaffected.
func validateRegistryCacheVM(v RegistryCacheVMSpec) []error {
	var errs []error
	if strings.TrimSpace(v.Template) == "" {
		errs = append(errs, errors.New(
			"registry_cache.vm: template is required — there is no safe default base image to guess"))
	}
	// Same 802.1Q range the pool members are held to. 0 means untagged, which
	// is legal but almost certainly a mistake here: an untagged cache VM lands
	// back on the LAN, which is the arrangement this block exists to escape.
	if v.VLANTag < 0 || v.VLANTag > 4094 {
		errs = append(errs, fmt.Errorf("registry_cache.vm: vlan_tag %d out of range (1-4094, or 0 for untagged)", v.VLANTag))
	}
	if v.Cores < 0 {
		errs = append(errs, errors.New("registry_cache.vm: cores must be >= 0"))
	}
	if v.MemoryMB < 0 {
		errs = append(errs, errors.New("registry_cache.vm: memory_mb must be >= 0"))
	}
	if v.DiskGB < 0 {
		errs = append(errs, errors.New("registry_cache.vm: disk_gb must be >= 0"))
	}
	return errs
}

// validateAbsURL requires an absolute http/https URL.
func validateAbsURL(field, raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("%s: %q is not a URL: %w", field, raw, err)
	}
	if u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return fmt.Errorf("%s: %q must be an absolute http(s) URL", field, raw)
	}
	return nil
}

func (c *Config) validateMonitoring() []error {
	if c.Monitoring == nil {
		return nil
	}
	var errs []error
	m := c.Monitoring
	if m.Cores < 0 || m.MemoryMB < 0 || m.DiskGB < 0 {
		errs = append(errs, errors.New("monitoring: cores/memory_mb/disk_gb must be >= 0"))
	}
	if m.RetentionDays < 0 {
		errs = append(errs, errors.New("monitoring: retention_days must be >= 0"))
	}
	// The monitoring hostname must not collide with a pool webhook hostname.
	for name, p := range c.Pools {
		if m.Hostname != "" && p.Hostname == m.Hostname {
			errs = append(errs, fmt.Errorf("monitoring: hostname %q already used by pool %q", m.Hostname, name))
		}
	}
	return errs
}

// needsProxmox reports whether the fleet touches the Proxmox backend at all:
// any VM-lifecycle pool, or a monitoring node (always a Proxmox VM). A fleet of
// nothing but SSH-enrolled bare metal never needs Proxmox creds.
func (c *Config) needsProxmox() bool {
	if c.Monitoring != nil {
		return true // monitoring runs on Proxmox
	}
	for _, p := range c.Pools {
		// Enrolled bare-metal pools (darwin, or linux with members) never
		// touch a hypervisor; anything else provisions VMs on Proxmox.
		if p.UsesEnroll() {
			continue
		}
		return true
	}
	return false
}

func (c *Config) validatePools(opts loadOptions) []error {
	var errs []error
	names := make([]string, 0, len(c.Pools))
	for name := range c.Pools {
		names = append(names, name)
	}
	sort.Strings(names)
	hostnames := map[string]string{}
	for _, name := range names {
		p := c.Pools[name]
		if !validPoolName(name) {
			errs = append(errs, fmt.Errorf("pool %q: name must be lowercase alphanumeric with hyphens", name))
		}
		if p.Hostname == "" {
			errs = append(errs, fmt.Errorf("pool %q: missing hostname", name))
		} else if prev, dup := hostnames[p.Hostname]; dup {
			errs = append(errs, fmt.Errorf("pool %q: hostname %q already used by pool %q", name, p.Hostname, prev))
		} else {
			hostnames[p.Hostname] = name
		}
		if p.Size < 0 {
			errs = append(errs, fmt.Errorf("pool %q: size must be >= 0", name))
		}
		if p.ActionsCache {
			if p.OS != "" && p.OS != "linux" {
				errs = append(errs, fmt.Errorf(
					"pool %q: actions_cache is Linux-only (ephemerd's LAN Actions cache does not run on %s); remove it from this pool", name, p.OS))
			}
			if !c.RegistryCache.ActionsActive() {
				errs = append(errs, fmt.Errorf(
					"pool %q: actions_cache = true needs [registry_cache.actions] enabled on a dedicated [registry_cache.vm]", name))
			}
		}
		switch p.OS {
		case "", "linux":
			// A linux pool with members enrolls existing bare metal over SSH
			// instead of provisioning PVE VMs — same lifecycle as darwin, so
			// the VM-placement knobs don't apply and size follows the list.
			if len(p.Members) > 0 {
				if p.Size != 0 && p.Size != len(p.Members) {
					errs = append(errs, fmt.Errorf("pool %q: linux pools with members take no size (member list is the size)", name))
				}
				if p.Template != "" || p.Node != "" || p.Storage != "" || p.VMStorage != "" {
					errs = append(errs, fmt.Errorf("pool %q: template/node/storage do not apply to enrolled (members = [...]) linux pools", name))
				}
			}
		case "windows":
			if p.Arch == "arm64" {
				errs = append(errs, fmt.Errorf("pool %q: windows/arm64 is not supported (no runner release asset)", name))
			}
			if p.Template == "" {
				errs = append(errs, fmt.Errorf("pool %q: windows pools require an explicit template (build it once with Cloudbase-Init + QEMU guest agent, sysprep, convert to template)", name))
			}
		case "darwin":
			if len(p.Members) == 0 {
				errs = append(errs, fmt.Errorf("pool %q: darwin pools require members = [\"host1\", ...] (bare metal is enrolled, not provisioned)", name))
			}
			if p.Arch == "amd64" {
				errs = append(errs, fmt.Errorf("pool %q: darwin/amd64 is not supported (only darwin_arm64 release assets exist)", name))
			}
			if p.Size != 0 && p.Size != len(p.Members) {
				errs = append(errs, fmt.Errorf("pool %q: darwin pools take no size (member list is the size)", name))
			}
			if p.Template != "" || p.Node != "" || p.Storage != "" || p.CloudInitStorage != "" || p.VMStorage != "" {
				errs = append(errs, fmt.Errorf("pool %q: template/node/storage do not apply to darwin pools", name))
			}
		default:
			errs = append(errs, fmt.Errorf("pool %q: os must be \"linux\", \"windows\", or \"darwin\", got %q", name, p.OS))
		}
		// members + ssh_* drive SSH bare-metal enrollment, valid for darwin and
		// linux pools but not windows (Hyper-V VMs only).
		if p.OS == "windows" && len(p.Members) > 0 {
			errs = append(errs, fmt.Errorf("pool %q: members is only valid for darwin or linux pools", name))
		}
		if p.OS == "windows" && (p.SSHUser != "" || p.SSHKey != "" || p.SSHPort != 0 || p.SSHPassword != "") {
			errs = append(errs, fmt.Errorf("pool %q: ssh_* settings are only valid for darwin or linux pools", name))
		}
		// ssh_* without a member list is meaningless (nothing to enroll).
		if len(p.Members) == 0 && (p.SSHUser != "" || p.SSHKey != "" || p.SSHPort != 0 || p.SSHPassword != "") {
			errs = append(errs, fmt.Errorf("pool %q: ssh_* settings require members = [\"host1\", ...]", name))
		}
		if p.OS != "darwin" && !p.VM.IsZero() {
			errs = append(errs, fmt.Errorf("pool %q: vm sizing ([vm.macos]/[vm.linux]) is only valid for darwin pools", name))
		}
		for vmName, vs := range map[string]VMSpec{"vm.macos": p.VM.MacOS, "vm.linux": p.VM.Linux} {
			if vs.CPUs < 0 || vs.MemoryMB < 0 || vs.DiskSizeGB < 0 {
				errs = append(errs, fmt.Errorf("pool %q: %s cpus/memory_mb/disk_size_gb must be >= 0", name, vmName))
			}
		}
		if p.OS != "windows" && !p.WindowsRunner.IsZero() {
			errs = append(errs, fmt.Errorf("pool %q: windows_runner ([runner.windows]) is only valid for windows pools", name))
		}
		if p.WindowsRunner.MemoryMB < 0 || p.WindowsRunner.CPUs < 0 {
			errs = append(errs, fmt.Errorf("pool %q: windows_runner memory_mb/cpus must be >= 0", name))
		}
		errs = append(errs, p.DispatchPolicy.validate(fmt.Sprintf("pool %q", name))...)
		for i, gt := range p.GitHubExtra {
			if gt.Owner == "" {
				errs = append(errs, fmt.Errorf("pool %q: github_extra[%d]: missing owner", name, i))
			}
			// The extra-owner token is a credential; a single-node command
			// never dispatches with it, so skip it under skipCredentials.
			if !opts.skipCredentials && gt.ResolvedToken() == "" {
				errs = append(errs, fmt.Errorf("pool %q: github_extra[%d] (%s): missing token (set token or token_env)", name, i, gt.Owner))
			}
			errs = append(errs, gt.DispatchPolicy.validate(fmt.Sprintf("pool %q: github_extra[%d]", name, i))...)
		}
		if p.JobTimeout != "" {
			if _, err := time.ParseDuration(p.JobTimeout); err != nil {
				errs = append(errs, fmt.Errorf("pool %q: invalid job_timeout %q: %w", name, p.JobTimeout, err))
			}
		}
		if p.OrphanGrace != "" {
			if _, err := time.ParseDuration(p.OrphanGrace); err != nil {
				errs = append(errs, fmt.Errorf("pool %q: invalid orphan_grace %q: %w", name, p.OrphanGrace, err))
			}
		}
		if p.DrainTimeout != "" {
			if _, err := time.ParseDuration(p.DrainTimeout); err != nil {
				errs = append(errs, fmt.Errorf("pool %q: invalid drain_timeout %q: %w", name, p.DrainTimeout, err))
			}
		}
		switch p.Arch {
		case "", "amd64", "arm64":
		default:
			errs = append(errs, fmt.Errorf("pool %q: arch must be \"amd64\" or \"arm64\", got %q", name, p.Arch))
		}
		switch p.Provider {
		case "", ProviderProxmox:
		default:
			errs = append(errs, fmt.Errorf("pool %q: provider must be \"proxmox\", got %q", name, p.Provider))
		}
		// vlan_tag is a Proxmox 802.1Q tag: 1–4094 (0 = unset/untagged, the
		// backward-compatible default; 4095 is reserved).
		if p.VLANTag < 0 || p.VLANTag > 4094 {
			errs = append(errs, fmt.Errorf("pool %q: vlan_tag must be between 1 and 4094 (0 = untagged), got %d", name, p.VLANTag))
		}
		// vlan_tag/firewall/allow_lan configure or exempt a hypervisor vNIC;
		// enrolled bare metal (darwin, or linux with members) has no vNIC mayfly
		// controls, so all three are meaningless there.
		if (p.VLANTag != 0 || p.Firewall) && p.UsesEnroll() {
			errs = append(errs, fmt.Errorf("pool %q: vlan_tag/firewall do not apply to enrolled (members = [...]) or darwin pools", name))
		}
		if p.AllowLAN && p.UsesEnroll() {
			errs = append(errs, fmt.Errorf("pool %q: allow_lan does not apply to enrolled (members = [...]) or darwin pools — they have no mayfly-controlled vNIC", name))
		}
		// Isolation-required guard: a pool that provisions Proxmox VMs must not
		// silently drop its runners onto the trusted flat LAN. It must either tag
		// them onto an isolated VLAN (vlan_tag) or explicitly accept the LAN
		// (allow_lan = true). This closes the fail-open hole where a new VM pool
		// lands un-isolated just by forgetting the tag. Enrolled bare metal and
		// darwin are exempt: they have no mayfly-controlled vNIC (they reject
		// vlan_tag above) and are isolated by other means.
		provisionsVMs := (p.OS == "" || p.OS == "linux" || p.OS == "windows") && !p.UsesEnroll()
		if provisionsVMs && p.ResolvedProvider() == ProviderProxmox && p.VLANTag == 0 && !p.AllowLAN {
			errs = append(errs, fmt.Errorf(
				"pool %q: provisions Proxmox VMs but sets no vlan_tag — its runners would join the trusted LAN. Set vlan_tag = <1-4094> to isolate the pool, or allow_lan = true to deliberately opt out",
				name))
		}
		// provider only applies to hypervisor VM pools; enrolled bare metal has
		// no hypervisor.
		if p.Provider != "" && len(p.Members) > 0 {
			errs = append(errs, fmt.Errorf("pool %q: provider does not apply to enrolled (members = [...]) pools", name))
		}
		if p.Provider != "" && p.OS == "darwin" {
			errs = append(errs, fmt.Errorf("pool %q: provider does not apply to darwin pools", name))
		}
	}
	return errs
}

func validPoolName(name string) bool {
	if name == "" || len(name) > 63 {
		return false
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9':
		case c == '-':
			if i == 0 || i == len(name)-1 {
				return false
			}
		default:
			return false
		}
	}
	return true
}
