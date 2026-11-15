package engine

// Tracking the ephemerd binary apart from the config fingerprint.
//
// A member drifts in two independent ways, and they need different fixes:
//
//   - its rendered config.toml / app.pem differ from what the pool now renders
//     -> push the files and restart ephemerd (the config tier, which cordons
//     the pool's webhooks while it waits for each node to go idle);
//   - it runs a different ephemerd release than [ephemerd] / ephemerd_version
//     selects -> upgrade it (the daemon's own self-upgrade drains in-process,
//     so it needs no webhook cordon at all).
//
// Until this split, ConfigHash folded the binary selection into the config
// fingerprint. A version bump therefore read as config drift on every member,
// and because `mayfly node upgrade` never touched the hash, upgrading the nodes
// did not clear it: plan offered to config-push and restart the entire fleet,
// cordoning every pool on the way, to fix nothing. Worse, for VM members that
// config tier never installs a binary at all, so the "fix" would have
// restarted each node on the version it already ran and then recorded it as
// upgraded.

import (
	"fmt"
	"strings"

	"github.com/luthermonson/mayfly/internal/config"
	"github.com/luthermonson/mayfly/internal/state"
)

// binaryURLPrefix marks a BinaryRef that names a pinned artifact rather than a
// release tag.
const binaryURLPrefix = "url:"

// BinaryRef names the ephemerd binary an [ephemerd] selection installs: the
// release tag, or "url:<binary_url>" when a specific artifact is pinned (the
// URL wins, matching resolveEphemerdURL).
func BinaryRef(src config.Ephemerd) string {
	if src.BinaryURL != "" {
		return binaryURLPrefix + src.BinaryURL
	}
	return src.Version
}

// DesiredBinary is the BinaryRef a member of the pool should run, honouring a
// per-pool ephemerd_version pin over the global [ephemerd] section.
func DesiredBinary(cfg *config.Config, spec config.PoolSpec) string {
	return BinaryRef(EphemerdSource(cfg.Ephemerd, spec))
}

func isURLRef(ref string) bool { return strings.HasPrefix(ref, binaryURLPrefix) }

// sameBinary compares two BinaryRefs. Release tags compare by their numeric
// version, so "v0.2.20" and "0.2.20" agree; pinned URLs compare exactly.
func sameBinary(a, b string) bool {
	if a == b {
		return true
	}
	if isURLRef(a) || isURLRef(b) {
		return false
	}
	return sameVersion(a, b)
}

// normalizeVersion pulls a release tag out of a daemon's self-reported version
// ("ephemerd version 0.2.20 (abc1234)" -> "v0.2.20"). ok is false when there is
// no semantic version in s.
func normalizeVersion(s string) (string, bool) {
	v, ok := parseVersion(s)
	if !ok {
		return "", false
	}
	return fmt.Sprintf("v%d.%d.%d", v[0], v[1], v[2]), true
}

// isLegacyConfigHash reports whether h was written by the old fingerprint,
// which included the binary selection.
func isLegacyConfigHash(h string) bool {
	return h != "" && !strings.HasPrefix(h, configHashPrefix)
}

// legacyHashMatches reports whether a pre-"c2:" ConfigHash recorded on m still
// matches the pool's current rendered config, under any binary selection it
// could plausibly have been taken with: the current global [ephemerd]
// settings (the old formula always used the global section), and each known
// version of the node — what state says it runs, plus any extra versions the
// caller has observed (a live probe, the version a node upgraded FROM).
//
// A match means the config itself has not drifted, whatever the binary did.
func legacyHashMatches(cfg *config.Config, st *state.Store, spec config.PoolSpec, pool *state.Pool, pem []byte, m state.Member, extra ...string) bool {
	type cand struct{ version, url string }
	cands := []cand{{cfg.Ephemerd.Version, cfg.Ephemerd.BinaryURL}}
	for _, ref := range append([]string{m.Ephemerd}, extra...) {
		switch {
		case ref == "":
		case isURLRef(ref):
			cands = append(cands, cand{cfg.Ephemerd.Version, strings.TrimPrefix(ref, binaryURLPrefix)})
		default:
			v := ref
			if n, ok := normalizeVersion(ref); ok {
				v = n
			}
			cands = append(cands, cand{v, ""}, cand{v, cfg.Ephemerd.BinaryURL})
		}
	}
	for _, c := range cands {
		if legacyConfigHash(cfg, st, spec, pool, pem, c.version, c.url) == m.ConfigHash {
			return true
		}
	}
	return false
}

// memberDrift is the per-member result of comparing state against config, with
// the two kinds of drift kept apart and each carrying its reason, so the plan
// can say WHY a member is marked rather than just that it is.
type memberDrift struct {
	Config       bool
	ConfigReason string
	Upgrade      bool
	FromBinary   string
	ToBinary     string
}

// diffMemberEphemerd decides config and binary drift for one member. desired
// is ConfigHash for the member's pool.
func diffMemberEphemerd(cfg *config.Config, st *state.Store, spec config.PoolSpec, pool *state.Pool, pem []byte, m state.Member, desired string) memberDrift {
	var d memberDrift
	switch {
	case m.ConfigHash == "":
		d.Config = true
		d.ConfigReason = "no config recorded for this member (ephemerd missing, or never configured by mayfly)"
	case isLegacyConfigHash(m.ConfigHash):
		if !legacyHashMatches(cfg, st, spec, pool, pem, m) {
			d.Config = true
			d.ConfigReason = "recorded fingerprint predates separate binary tracking and matches neither the current config nor any known " +
				"ephemerd version, so a config change cannot be told apart from a version bump; if the node's config is known-good, " +
				"`mayfly node reconfigure --no-restart <node>` re-records it without a restart"
		}
	case m.ConfigHash != desired:
		d.Config = true
		d.ConfigReason = "rendered config.toml / app.pem changed"
	}

	want := DesiredBinary(cfg, spec)
	// An unknown binary is not drift: mayfly cannot say the node is wrong, and
	// the refresh's live probe fills it in whenever the node answers.
	if m.Ephemerd != "" && want != "" && !sameBinary(m.Ephemerd, want) {
		d.Upgrade = true
		d.FromBinary = m.Ephemerd
		d.ToBinary = want
	}
	return d
}

// MigrateConfigHash rewrites a member's pre-"c2:" ConfigHash in the current
// format when it still matches the pool's config (see legacyHashMatches), so
// later version bumps are judged on the binary alone. It reports whether it
// rewrote anything; a hash that does not match is left for the planner to
// flag. knownVersions are extra versions the node is known to have run.
func MigrateConfigHash(cfg *config.Config, st *state.Store, spec config.PoolSpec, pool *state.Pool, member *state.Member, pem []byte, knownVersions ...string) bool {
	if !isLegacyConfigHash(member.ConfigHash) {
		return false
	}
	if !legacyHashMatches(cfg, st, spec, pool, pem, *member, knownVersions...) {
		return false
	}
	member.ConfigHash = ConfigHash(cfg, st, spec, pool, pem)
	return true
}

// migrateLegacyHashes runs MigrateConfigHash over every member of every
// configured pool. In memory only; apply persists it with the rest of state.
func migrateLegacyHashes(cfg *config.Config, st *state.Store, pem []byte) {
	for i := range st.Pools {
		pool := &st.Pools[i]
		spec, ok := resolvedPoolSpec(cfg, pool.Name)
		if !ok {
			continue
		}
		for j := range pool.Members {
			MigrateConfigHash(cfg, st, spec, pool, &pool.Members[j], pem)
		}
	}
}

// RecordUpgrade updates a member's state after a verified ephemerd upgrade to
// the binary src selects. preVersion is what the node reported before the
// upgrade; it is the binary a legacy ConfigHash was most likely taken with, so
// it lets that hash be migrated instead of reading as config drift. pem may be
// nil when the caller has no GitHub App key loaded, in which case only the
// binary is recorded.
func RecordUpgrade(cfg *config.Config, st *state.Store, pool *state.Pool, member *state.Member, src config.Ephemerd, preVersion string, pem []byte) {
	if pem != nil {
		if spec, ok := resolvedPoolSpec(cfg, pool.Name); ok {
			MigrateConfigHash(cfg, st, spec, pool, member, pem, preVersion)
		}
	}
	member.Ephemerd = BinaryRef(src)
}

// adoptLiveVersion corrects a member's recorded binary from what the node
// itself reports, so an `ephemerd upgrade` run directly on the box (or a
// state file that predates the field) is reconciled rather than flagged.
// want is the pool's DesiredBinary. A pinned binary_url cannot be confirmed
// from a version string, so a URL ref is only replaced when config no longer
// pins one. It returns the previous value and whether it changed.
func adoptLiveVersion(m *state.Member, reported, want string) (string, bool) {
	live, ok := normalizeVersion(reported)
	if !ok || isURLRef(want) {
		return m.Ephemerd, false
	}
	if m.Ephemerd != "" && !isURLRef(m.Ephemerd) && sameBinary(m.Ephemerd, live) {
		return m.Ephemerd, false
	}
	old := m.Ephemerd
	m.Ephemerd = live
	return old, true
}

// describeBinary renders a BinaryRef for plan output.
func describeBinary(ref string) string {
	switch {
	case ref == "":
		return "unknown"
	case isURLRef(ref):
		return "binary_url " + strings.TrimPrefix(ref, binaryURLPrefix)
	default:
		return ref
	}
}
