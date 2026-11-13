package ephemerd

import (
	"bytes"
	"fmt"
)

// ---------------------------------------------------------------------------
// These key names are reconciled against what ships in ephemerd's
// [registry_mirror] config (pkg/config RegistryMirrorConfig). Everything mayfly
// renders for the mirror is spelled out in the constants below and written by
// exactly one function (writeRegistryMirror) — if ephemerd's names ever change,
// change them here and nowhere else. Nothing outside this file knows the wire
// format.
//
// Deliberately NOT rendered:
//   - registries / mirrors: ephemerd defaults to mirroring docker.io, which is
//     what the fleet cache proxies. A per-host override belongs in the node's
//     own config, not in something mayfly generates for every node.
//   - an "insecure" flag: ephemerd has no such key. It derives plaintext from
//     the endpoint's scheme, so an http:// LAN endpoint already works.
//   - forward_credentials: ephemerd defaults it off so a cache answering with
//     an auth challenge cannot harvest a job's registry token in cleartext.
//     Mayfly must not quietly turn that on.
// ---------------------------------------------------------------------------

const (
	// registryMirrorSection is the TOML table ephemerd reads the mirror from.
	registryMirrorSection = "registry_mirror"
	// registryMirrorKeyEnabled turns the mirror on.
	registryMirrorKeyEnabled = "enabled"
	// registryMirrorKeyEndpoint is the cache's base URL.
	registryMirrorKeyEndpoint = "endpoint"
	// registryMirrorKeyFallbackToOrigin falls back to the upstream registry when
	// the mirror is unreachable instead of failing the pull. ephemerd defaults
	// this to true; mayfly writes it explicitly so the node's config states the
	// fleet's intent rather than relying on a default.
	registryMirrorKeyFallbackToOrigin = "fallback_to_origin"
)

// RegistryMirror points a node's container runtime at the fleet's pull-through
// cache. A zero value renders nothing at all, so a fleet without a cache emits
// the same config.toml it did before this existed.
type RegistryMirror struct {
	// Endpoint is the cache's base URL, e.g. "http://10.0.0.45:5000". Empty
	// disables the mirror entirely — mayfly leaves it empty when the cache is
	// off or when the utility host has no address yet.
	Endpoint string
	// FailOpen falls back to the upstream registry when the mirror is down.
	// Rendered as ephemerd's fallback_to_origin.
	FailOpen bool
}

// Enabled reports whether anything should be rendered.
func (m RegistryMirror) Enabled() bool { return m.Endpoint != "" }

// NewRegistryMirror builds a mirror input. A plaintext http:// endpoint needs no
// extra flag — ephemerd reads the scheme off the endpoint itself.
func NewRegistryMirror(endpoint string, failOpen bool) RegistryMirror {
	return RegistryMirror{
		Endpoint: endpoint,
		FailOpen: failOpen,
	}
}

// writeRegistryMirror emits the mirror block. This is the single render site
// for the mirror — see the TODO at the top of this file.
func writeRegistryMirror(buf *bytes.Buffer, m RegistryMirror) {
	if !m.Enabled() {
		return
	}
	fmt.Fprintln(buf)
	fmt.Fprintf(buf, "[%s]\n", registryMirrorSection)
	fmt.Fprintf(buf, "%s = true\n", registryMirrorKeyEnabled)
	fmt.Fprintf(buf, "%s = %q\n", registryMirrorKeyEndpoint, m.Endpoint)
	fmt.Fprintf(buf, "%s = %t\n", registryMirrorKeyFallbackToOrigin, m.FailOpen)
}
