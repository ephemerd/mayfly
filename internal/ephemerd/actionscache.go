package ephemerd

import (
	"bytes"
	"fmt"
)

// Key names reconciled against ephemerd's [actions_cache] config
// (pkg/config/actionscache.go, ephemerd #217). As with the registry mirror,
// this file is the only place that knows the wire format; ephemerd drops
// unknown TOML keys silently, so a typo here would render a cache that never
// turns on.
//
// Deliberately NOT rendered, leaving ephemerd's defaults:
//   - port (8089): the shim's listener on the bridge gateway.
//   - max_size_gb_per_repo / max_entry_gb: per-repo eviction is ephemerd's
//     business; mayfly bounds the registry's disk on the cache VM instead.
//   - upstream_results_url: GitHub's results service, for artifact pass-through.
//   - password_env: there is no mechanism for mayfly to put an environment
//     variable into the ephemerd service, so the password is rendered into
//     config.toml, which every provisioning path writes 0600 root — the same
//     treatment as the github_extra PATs and the tunnel token already get.
const (
	actionsCacheSection     = "actions_cache"
	actionsCacheKeyEnabled  = "enabled"
	actionsCacheKeyRegistry = "registry"
	actionsCacheKeyUsername = "username"
	actionsCacheKeyPassword = "password"
)

// ActionsCache points a node's ephemerd at the writable Actions registry on the
// cache VM. A zero value renders nothing.
type ActionsCache struct {
	// Registry is the registry's base URL, e.g. "http://192.0.2.224:5001".
	// Empty disables the block — mayfly leaves it empty until the cache VM has
	// an address, and for pools that did not opt in.
	Registry string
	Username string
	Password string
}

// Enabled reports whether anything should be rendered.
func (a ActionsCache) Enabled() bool { return a.Registry != "" }

// writeActionsCache emits the [actions_cache] block. Linux nodes only: the
// feature is Linux-only in ephemerd, and config validation refuses it on other
// pools; this guard keeps a stray input from rendering it anyway.
func writeActionsCache(buf *bytes.Buffer, a ActionsCache, osName string) {
	if !a.Enabled() || (osName != "" && osName != "linux") {
		return
	}
	fmt.Fprintln(buf)
	fmt.Fprintf(buf, "[%s]\n", actionsCacheSection)
	fmt.Fprintf(buf, "%s = true\n", actionsCacheKeyEnabled)
	fmt.Fprintf(buf, "%s = %q\n", actionsCacheKeyRegistry, a.Registry)
	fmt.Fprintf(buf, "%s = %q\n", actionsCacheKeyUsername, a.Username)
	fmt.Fprintf(buf, "%s = %q\n", actionsCacheKeyPassword, a.Password)
}
