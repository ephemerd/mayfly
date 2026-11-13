package ephemerd

import (
	"bytes"
	"fmt"
)

type ConfigInputs struct {
	Owner          string
	Repos          []string
	AppID          int64
	InstallationID int64
	PrivateKeyPath string
	MaxConcurrent  int    // 0 = ephemerd default
	JobTimeout     string // [runner] job_timeout; "" = ephemerd default (2h)
	OrphanGrace    string // [runner.orphan_sweep] grace; "" = ephemerd default (10m)
	// webhook via ephemerd-managed cloudflared:
	TunnelToken    string
	TunnelHostname string
	WebhookSecret  string
	// MetricsEnabled exposes ephemerd's Prometheus /metrics endpoint on
	// MetricsPort (for the monitoring node to scrape over the LAN).
	MetricsEnabled bool
	MetricsPort    int // defaults to 9090 when MetricsEnabled and unset

	// MetricsBindAddr is the interface ephemerd binds /metrics to.
	//
	// It MUST be set for the monitoring node to scrape anything. ephemerd
	// defaults this to 127.0.0.1 on purpose -- the endpoint is
	// unauthenticated -- so a config that omits it produces a daemon
	// listening only on loopback, and every scrape from another host gets
	// "connection refused" while the node itself reports perfectly healthy.
	//
	// That is not theoretical: the fleet ran that way. All five runner
	// targets showed DOWN in Prometheus, which is why a registry-cache
	// outage went unnoticed for four weeks -- there were no metrics to
	// notice it with.
	//
	// Empty renders nothing, preserving the loopback default for anyone who
	// has not opted in. "0.0.0.0" is the usual value; firewall the port
	// (and/or set ephemerd's tls_cert/tls_key) when you do, because it is
	// unauthenticated.
	MetricsBindAddr string
	// DefaultImageLinux/Windows override the runner container image per job OS
	// ([github] default_image_linux/windows). Empty = ephemerd's built-in.
	DefaultImageLinux   string
	DefaultImageWindows string
	// DispatchPolicy gates which incoming workflow_job webhooks may dispatch a
	// runner for the primary [github] target, rendered as
	// [github.dispatch_policy]. A zero value renders NOTHING — see
	// writeDispatchPolicy for why an empty list must never be emitted.
	DispatchPolicy DispatchPolicy
	// Dind enables the in-daemon Docker shim; DindAllowPrivileged additionally
	// permits privileged job containers.
	DindEnabled         bool
	DindAllowPrivileged bool
	// RegistryMirror points image pulls at the fleet's pull-through cache. A
	// zero value renders nothing. Unlike the gateway cache proxies below this
	// applies on every OS: the mirror is an ordinary LAN endpoint the host's
	// container runtime dials, not a bridge-gateway listener, so windows jobs
	// can reach it.
	RegistryMirror RegistryMirror
	// The *ProxyEnabled toggles turn on ephemerd's per-runner package caches:
	// Go modules (module), Rust crates (cargo), PHP Packagist (composer), npm,
	// PyPI (pip), Dart/Flutter pub, and GitHub release assets (ghrel). Every one
	// of them is a gateway listener and so is skipped when OS == "windows" (see
	// OS below). Rendering order is fixed by cacheProxies — see there.
	ModuleProxyEnabled   bool
	CargoProxyEnabled    bool
	ComposerProxyEnabled bool
	NpmProxyEnabled      bool
	PipProxyEnabled      bool
	PubProxyEnabled      bool
	GhrelProxyEnabled    bool
	// ActionsCache turns on ephemerd's LAN Actions cache against the cache
	// VM's writable registry. A zero value renders nothing; linux only.
	ActionsCache ActionsCache
	// OS is the pool's job OS ("linux", "windows", "darwin"). Gateway cache
	// proxies are skipped for windows: they listen on the bridge gateway, which
	// windows Hyper-V-isolated job containers cannot reach, so injecting GOPROXY
	// there hangs `go build`. dind and image overrides still apply on windows.
	OS string
	// ExtraTargets are secondary GitHub owners this daemon also serves, each
	// rendered as a [[github_extra]] block with its own PAT.
	ExtraTargets []ExtraTarget
	// MacOSVM and LinuxVM size ephemerd's per-job guest VMs on a darwin host —
	// rendered as [vm.macos] and [vm.linux]. Zero-valued VMResources (and each
	// zero field within) is omitted, leaving ephemerd's built-in default.
	MacOSVM VMResources
	LinuxVM VMResources
	// WindowsRunnerMemoryMB and WindowsRunnerCPUs size the Hyper-V-isolated
	// container each Windows job runs in — rendered as [runner.windows]. Zero
	// omits the key, leaving ephemerd's default (4096 MB / 2 CPU).
	WindowsRunnerMemoryMB int
	WindowsRunnerCPUs     int
}

// ExtraTarget is a secondary GitHub owner rendered as a [[github_extra]] block.
// Token is a PAT (already resolved from config/env by the caller).
type ExtraTarget struct {
	Owner string
	Repos []string
	Token string
	// DispatchPolicy is this target's own allowlist, rendered as
	// [github_extra.dispatch_policy] under its [[github_extra]] element.
	// ephemerd reads a policy per target with no inheritance from [github], so
	// the caller must supply it explicitly; a zero value renders nothing and
	// leaves the target ungated.
	DispatchPolicy DispatchPolicy
}

// DispatchPolicy is ephemerd's [github.dispatch_policy] block: an opt-in
// allowlist over incoming workflow_job webhooks. Field names track ephemerd's
// DispatchPolicyConfig struct tags exactly (allowed_repos / required_labels) —
// ephemerd silently ignores unknown TOML keys, so a typo here renders a config
// that looks right and enforces nothing.
type DispatchPolicy struct {
	AllowedRepos   []string
	RequiredLabels []string
}

// isZero reports whether the policy restricts nothing, matching ephemerd's own
// DispatchPolicyConfig.IsZero.
func (d DispatchPolicy) isZero() bool {
	return len(d.AllowedRepos) == 0 && len(d.RequiredLabels) == 0
}

// VMResources is per-VM sizing for a darwin host's guest VMs. A zero field is
// omitted from the rendered section so ephemerd applies its own default.
type VMResources struct {
	// Enabled, when non-nil, is rendered as the section's `enabled` key —
	// false stops the host serving jobs through this VM (ephemerd honors it
	// from > v0.1.4; older daemons parse and ignore). nil omits the key,
	// keeping ephemerd's platform default.
	Enabled    *bool
	CPUs       int
	MemoryMB   int
	DiskSizeGB int
}

func (r VMResources) isZero() bool { return r == VMResources{} }

// RenderConfig produces the config.toml ephemerd reads at /var/lib/ephemerd/config.toml.
// Repos may be empty for org-level runners.
func RenderConfig(in ConfigInputs) (string, error) {
	if in.Owner == "" {
		return "", fmt.Errorf("ephemerd config: missing owner")
	}
	if in.AppID == 0 || in.InstallationID == 0 || in.PrivateKeyPath == "" {
		return "", fmt.Errorf("ephemerd config: missing github app credentials")
	}
	if in.TunnelToken != "" {
		if in.TunnelHostname == "" {
			return "", fmt.Errorf("ephemerd config: tunnel token set but missing tunnel hostname")
		}
		if in.WebhookSecret == "" {
			return "", fmt.Errorf("ephemerd config: tunnel token set but missing webhook secret")
		}
	}
	for i, t := range in.ExtraTargets {
		if t.Owner == "" {
			return "", fmt.Errorf("ephemerd config: github_extra[%d]: missing owner", i)
		}
		if t.Token == "" {
			return "", fmt.Errorf("ephemerd config: github_extra[%d] (%s): missing token", i, t.Owner)
		}
	}
	var buf bytes.Buffer
	fmt.Fprintln(&buf, "[github]")
	fmt.Fprintf(&buf, "owner = %q\n", in.Owner)
	writeRepos(&buf, in.Repos)
	fmt.Fprintf(&buf, "app_id = %d\n", in.AppID)
	fmt.Fprintf(&buf, "installation_id = %d\n", in.InstallationID)
	fmt.Fprintf(&buf, "private_key_path = %q\n", in.PrivateKeyPath)
	if in.DefaultImageLinux != "" {
		fmt.Fprintf(&buf, "default_image_linux = %q\n", in.DefaultImageLinux)
	}
	if in.DefaultImageWindows != "" {
		fmt.Fprintf(&buf, "default_image_windows = %q\n", in.DefaultImageWindows)
	}
	// Must follow every bare [github] key and precede the [[github_extra]]
	// array: in TOML a sub-table header closes its parent to further bare keys.
	writeDispatchPolicy(&buf, "github.dispatch_policy", in.DispatchPolicy)
	for _, t := range in.ExtraTargets {
		fmt.Fprintln(&buf)
		fmt.Fprintln(&buf, "[[github_extra]]")
		fmt.Fprintf(&buf, "owner = %q\n", t.Owner)
		writeRepos(&buf, t.Repos)
		fmt.Fprintf(&buf, "token = %q\n", t.Token)
		// Inherit the node's default images. ephemerd builds a SEPARATE
		// provider per target off that target's own block, and resolves a
		// job's image from the provider that served it — so an extra target
		// with no defaults here silently falls all the way through to the
		// runtime's host-aware fallback (bare servercore on Windows, which
		// has no toolchain at all), while the primary owner's repos get the
		// fleet CI image. Observed on luthermonson/shipmates: every Windows
		// job ran on servercore and re-downloaded its Go toolchain, which is
		// what made actions/setup-go time out.
		//
		// A per-target override is not expressible in mayfly's config today;
		// when it is, prefer it over this inheritance.
		if in.DefaultImageLinux != "" {
			fmt.Fprintf(&buf, "default_image_linux = %q\n", in.DefaultImageLinux)
		}
		if in.DefaultImageWindows != "" {
			fmt.Fprintf(&buf, "default_image_windows = %q\n", in.DefaultImageWindows)
		}
		// Same reasoning as the images above, applied to the dispatch allowlist:
		// ephemerd applies each target's OWN policy with no inheritance from
		// [github], so a target left unset here dispatches for anything it
		// tracks even when the primary owner is gated. The header binds to the
		// most recently opened [[github_extra]] element, so it must stay last in
		// the block — a bare key written after it would land in the sub-table.
		writeDispatchPolicy(&buf, "github_extra.dispatch_policy", t.DispatchPolicy)
	}
	if in.TunnelToken != "" {
		fmt.Fprintln(&buf)
		fmt.Fprintln(&buf, "[webhook]")
		fmt.Fprintln(&buf, "tunnel = \"cloudflared\"")
		fmt.Fprintf(&buf, "cloudflared_token = %q\n", in.TunnelToken)
		fmt.Fprintf(&buf, "cloudflared_hostname = %q\n", in.TunnelHostname)
		fmt.Fprintf(&buf, "secret = %q\n", in.WebhookSecret)
		fmt.Fprintln(&buf, "port = 8080")
		// Every mayfly node is a pool member (a pool of one is still a pool):
		// adopt-or-create webhook registration, no deregister on shutdown, no
		// stale-hook sweep. Mayfly owns hook cleanup via pool destroy.
		fmt.Fprintln(&buf, "pool = true")
	}
	if in.MaxConcurrent > 0 || in.JobTimeout != "" {
		fmt.Fprintln(&buf)
		fmt.Fprintln(&buf, "[runner]")
		if in.MaxConcurrent > 0 {
			fmt.Fprintf(&buf, "max_concurrent = %d\n", in.MaxConcurrent)
		}
		if in.JobTimeout != "" {
			fmt.Fprintf(&buf, "job_timeout = %q\n", in.JobTimeout)
		}
	}
	if in.OrphanGrace != "" {
		fmt.Fprintln(&buf)
		fmt.Fprintln(&buf, "[runner.orphan_sweep]")
		fmt.Fprintf(&buf, "grace = %q\n", in.OrphanGrace)
	}
	writeVMSection(&buf, "vm.macos", in.MacOSVM)
	writeVMSection(&buf, "vm.linux", in.LinuxVM)
	if in.WindowsRunnerMemoryMB > 0 || in.WindowsRunnerCPUs > 0 {
		fmt.Fprintln(&buf)
		fmt.Fprintln(&buf, "[runner.windows]")
		if in.WindowsRunnerMemoryMB > 0 {
			fmt.Fprintf(&buf, "memory_mb = %d\n", in.WindowsRunnerMemoryMB)
		}
		if in.WindowsRunnerCPUs > 0 {
			fmt.Fprintf(&buf, "cpus = %d\n", in.WindowsRunnerCPUs)
		}
	}
	if in.MetricsEnabled {
		port := in.MetricsPort
		if port == 0 {
			port = 9090
		}
		fmt.Fprintln(&buf)
		fmt.Fprintln(&buf, "[metrics]")
		fmt.Fprintln(&buf, "enabled = true")
		fmt.Fprintf(&buf, "port = %d\n", port)
		// Omitted when empty so an existing fleet renders byte-identical
		// config and sees no drift; see MetricsBindAddr.
		if in.MetricsBindAddr != "" {
			fmt.Fprintf(&buf, "bind_addr = %q\n", in.MetricsBindAddr)
		}
	}
	if in.DindEnabled {
		fmt.Fprintln(&buf)
		fmt.Fprintln(&buf, "[dind]")
		fmt.Fprintln(&buf, "enabled = true")
		if in.DindAllowPrivileged {
			fmt.Fprintln(&buf, "allow_privileged = true")
		}
	}
	writeRegistryMirror(&buf, in.RegistryMirror)
	// Gateway cache proxies bind the bridge gateway, which windows Hyper-V job
	// containers can't route to — injecting their env (e.g. GOPROXY) there hangs
	// the build. Only emit them for non-windows pools.
	if in.OS != "windows" {
		for _, p := range cacheProxies(in) {
			if !p.enabled {
				continue
			}
			fmt.Fprintln(&buf)
			fmt.Fprintf(&buf, "[%s]\n", p.section)
			fmt.Fprintln(&buf, "enabled = true")
		}
	}
	// After the gateway proxies so a node that does not opt in renders bytes
	// identical to before this existed.
	writeActionsCache(&buf, in.ActionsCache, in.OS)
	fmt.Fprintln(&buf)
	fmt.Fprintln(&buf, "[log]")
	fmt.Fprintln(&buf, "level = \"info\"")
	return buf.String(), nil
}

// cacheProxy pairs an ephemerd cache-proxy section name with its toggle.
type cacheProxy struct {
	section string
	enabled bool
}

// cacheProxies lists every gateway package-cache proxy in the FIXED order they
// are rendered. The order is load-bearing twice over and must not be "tidied":
//
//   - The rendered config.toml is fingerprinted by the engine's ConfigHash, so a
//     reordering rewrites every node's hash and makes `mayfly plan` report config
//     drift on the whole fleet for a change that alters nothing semantically.
//   - module/cargo/composer must stay first, in that order, because that is what
//     the fleet already renders today. New proxies are APPENDED, never
//     interleaved, so a fleet that enables none of them renders bytes identical
//     to before they existed.
//
// Returning a slice (not iterating a map) is the point: map iteration order is
// randomized per run, which would produce a different hash on every plan.
func cacheProxies(in ConfigInputs) []cacheProxy {
	return []cacheProxy{
		{"module_proxy", in.ModuleProxyEnabled},
		{"cargo_proxy", in.CargoProxyEnabled},
		{"composer_proxy", in.ComposerProxyEnabled},
		{"npm_proxy", in.NpmProxyEnabled},
		{"pip_proxy", in.PipProxyEnabled},
		{"pub_proxy", in.PubProxyEnabled},
		{"ghrel_proxy", in.GhrelProxyEnabled},
	}
}

// writeVMSection emits a [vm.macos]/[vm.linux] block sizing a darwin host's
// guest VM. Each field is written only when > 0, so an unset field keeps
// ephemerd's own default; a fully-zero VMResources emits nothing at all.
func writeVMSection(buf *bytes.Buffer, section string, r VMResources) {
	if r.isZero() {
		return
	}
	fmt.Fprintln(buf)
	fmt.Fprintf(buf, "[%s]\n", section)
	if r.Enabled != nil {
		fmt.Fprintf(buf, "enabled = %t\n", *r.Enabled)
	}
	if r.CPUs > 0 {
		fmt.Fprintf(buf, "cpus = %d\n", r.CPUs)
	}
	if r.MemoryMB > 0 {
		fmt.Fprintf(buf, "memory_mb = %d\n", r.MemoryMB)
	}
	if r.DiskSizeGB > 0 {
		fmt.Fprintf(buf, "disk_size_gb = %d\n", r.DiskSizeGB)
	}
}

// writeDispatchPolicy emits a [<section>] block for an ephemerd dispatch
// allowlist, or nothing at all when the policy restricts nothing.
//
// "Unset" and "empty list" are NOT interchangeable to a reader even though
// ephemerd treats them alike. ephemerd's dispatchAllowed guards each field with
// `if len(...) > 0`, so `allowed_repos = []` means "no restriction", the exact
// opposite of the "allow nothing" an operator would read it as. Emitting an
// empty list would therefore put a line in every node's config.toml that looks
// like a lockdown and enforces nothing — so unset renders no key, and a fully
// unset policy renders no section header either.
func writeDispatchPolicy(buf *bytes.Buffer, section string, d DispatchPolicy) {
	if d.isZero() {
		return
	}
	fmt.Fprintln(buf)
	fmt.Fprintf(buf, "[%s]\n", section)
	// Key names must match ephemerd's DispatchPolicyConfig struct tags exactly;
	// unknown TOML keys are dropped without a word.
	writeStringList(buf, "allowed_repos", d.AllowedRepos)
	writeStringList(buf, "required_labels", d.RequiredLabels)
}

// writeStringList emits `key = ["a", "b"]` when vals is non-empty, and nothing
// otherwise.
func writeStringList(buf *bytes.Buffer, key string, vals []string) {
	if len(vals) == 0 {
		return
	}
	fmt.Fprintf(buf, "%s = [", key)
	for i, v := range vals {
		if i > 0 {
			fmt.Fprint(buf, ", ")
		}
		fmt.Fprintf(buf, "%q", v)
	}
	fmt.Fprintln(buf, "]")
}

// writeRepos emits a `repos = [...]` line when repos is non-empty; org/user
// level targets (empty repos) omit the key entirely.
func writeRepos(buf *bytes.Buffer, repos []string) {
	if len(repos) == 0 {
		return
	}
	fmt.Fprint(buf, "repos = [")
	for i, r := range repos {
		if i > 0 {
			fmt.Fprint(buf, ", ")
		}
		fmt.Fprintf(buf, "%q", r)
	}
	fmt.Fprintln(buf, "]")
}
