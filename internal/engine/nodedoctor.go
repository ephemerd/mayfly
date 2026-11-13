package engine

// Health battery for `mayfly node doctor`.
//
// This session repeatedly hand-verified a node's health and isolation with a
// scattered set of `node exec` probes: is ephemerd up and on the right version,
// is bridged traffic being filtered (net.bridge.bridge-nf-call-iptables=1), is
// the cross-container DROP chain (EPHEMERD-C2C) installed, does DNS resolve, and
// is there disk headroom left. `node doctor` freezes that battery into one
// command.
//
// The design goal here is testability: each check is a value carrying the argv
// to run and a pure Eval that turns the probe's output into pass/fail plus a
// one-line detail. The command layer runs the argv over whatever transport the
// node uses and feeds the result back to Eval, so a fake NodeExecutor plus a
// table of raw outputs exercises every verdict without touching a real node.
//
// A note on privilege and context: these run over the same channel as every
// other node subcommand — the guest agent (root) on a VM, the pool's SSH user on
// bare metal — so no sudo is added, matching `node status`/`node drain`. The DNS
// and iptables checks read the HOST's view; a runner container has its own
// netns, so a green host DNS check does not by itself prove in-container
// resolution (that is called out in the DNS hint).

import "strings"

// ephemerdDataDir is where ephemerd keeps its state on unix nodes; disk
// headroom is measured against the filesystem holding it.
const ephemerdDataDir = "/var/lib/ephemerd"

// diskUsedFailPercent is the fill level at or above which the disk-headroom
// check fails. Windows-node-disk-full outages have wedged runners; flag it well
// before the partition is full.
const diskUsedFailPercent = 90

// DoctorCheck is one health probe: a name, the argv to run on the node, a
// remediation Hint printed on failure, and a pure Eval that renders the probe's
// result into a verdict. A check with a non-empty NA is skipped without running
// (the probe does not exist on that OS) and NA explains why.
type DoctorCheck struct {
	Name string
	Argv []string
	Hint string
	NA   string
	Eval func(NodeExecResult) (pass bool, detail string)
}

// NodeDoctorChecks builds the battery to run on a node running osName.
//
// Linux hypervisor nodes get the full set. windows and darwin get a sensible
// subset — ephemerd active + version — with the linux-only bridge/iptables/DNS
// checks marked n/a rather than run, because sysctl, iptables, and getent are
// not how (or where) those platforms answer those questions.
func NodeDoctorChecks(osName string) []DoctorCheck {
	active := DoctorCheck{
		Name: "ephemerd active",
		Argv: EphemerdArgv(osName, "status"),
		Hint: "start it: mayfly node status <node>, then check `" + EphemerdExe(osName) + " status` on the box",
		Eval: evalEphemerdActive,
	}
	version := DoctorCheck{
		Name: "ephemerd version",
		Argv: EphemerdVersionArgv(osName),
		Hint: "daemon did not report a parseable version; it may be wedged — restart the service",
		Eval: evalEphemerdVersion,
	}

	if osName != "linux" {
		na := "n/a on " + osName + " (linux hypervisor nodes only)"
		return []DoctorCheck{
			active,
			version,
			{Name: "bridge netfilter", NA: na},
			{Name: "c2c drop chain", NA: na},
			{Name: "dns resolves", NA: na},
			{Name: "disk headroom", NA: na},
		}
	}

	return []DoctorCheck{
		active,
		version,
		{
			Name: "bridge netfilter",
			Argv: []string{"sysctl", "-n", "net.bridge.bridge-nf-call-iptables"},
			Hint: "set net.bridge.bridge-nf-call-iptables=1 (br_netfilter) or bridged runner traffic bypasses the firewall",
			Eval: evalBridgeNetfilter,
		},
		{
			Name: "c2c drop chain",
			Argv: []string{"iptables", "-S", "EPHEMERD-C2C"},
			Hint: "EPHEMERD-C2C DROP chain missing — cross-container isolation is off; restart ephemerd to reinstall it",
			Eval: evalC2CDropChain,
		},
		{
			// Scoped "host" deliberately: getent runs in the host netns, and this
			// fleet's real DNS failures are in-CONTAINER (CNI conflist / c2c), which
			// a green host result says nothing about. The name and both the PASS
			// detail and the fail Hint keep that caveat visible either way.
			Name: "host dns resolves",
			Argv: []string{"getent", "hosts", "github.com"},
			Hint: "host cannot resolve github.com — check /etc/resolv.conf; note this is the HOST netns, not the runner container (CNI conflist / c2c DNS)",
			Eval: evalDNSResolves,
		},
		{
			Name: "disk headroom",
			Argv: []string{"df", "-P", ephemerdDataDir},
			Hint: "the ephemerd data filesystem is nearly full — grow the guest partition (mayfly grows the disk but not the partition)",
			Eval: evalDiskHeadroom,
		},
	}
}

// evalEphemerdActive treats a zero exit from `ephemerd status` as up.
func evalEphemerdActive(res NodeExecResult) (bool, string) {
	if res.ExitCode == 0 {
		return true, "responding"
	}
	return false, "status exited " + itoaNonNeg(res.ExitCode)
}

// evalEphemerdVersion passes when the daemon reports a version we can parse —
// the same signal probeEphemerdVersion uses to decide a daemon is alive and
// modern enough to answer for itself.
func evalEphemerdVersion(res NodeExecResult) (bool, string) {
	out := strings.TrimSpace(res.Stdout + res.Stderr)
	if v, ok := parseVersion(out); ok {
		return true, versionTriple(v)
	}
	return false, "no version reported"
}

// evalBridgeNetfilter passes only when `sysctl -n` prints exactly 1.
func evalBridgeNetfilter(res NodeExecResult) (bool, string) {
	got := strings.TrimSpace(res.Stdout)
	if got == "1" {
		return true, "= 1"
	}
	if got == "" {
		got = "unset (br_netfilter not loaded?)"
	}
	return false, "= " + got
}

// evalC2CDropChain passes when the chain listing contains a DROP rule. A missing
// chain makes iptables exit non-zero with nothing on stdout, which also fails.
func evalC2CDropChain(res NodeExecResult) (bool, string) {
	if res.ExitCode != 0 {
		return false, "chain not present"
	}
	if strings.Contains(res.Stdout, "DROP") {
		return true, "DROP rule present"
	}
	return false, "chain present but no DROP rule"
}

// evalDNSResolves passes when getent found at least one address (exit 0, output
// non-empty).
func evalDNSResolves(res NodeExecResult) (bool, string) {
	if res.ExitCode == 0 && strings.TrimSpace(res.Stdout) != "" {
		// Carry the host-scope caveat into the PASS detail: a green result here
		// is host resolution only, not proof the runner container can resolve.
		return true, "github.com -> " + firstField(res.Stdout) + " (host netns; not in-container)"
	}
	return false, "no answer"
}

// evalDiskHeadroom parses `df -P` output and fails when the data filesystem is
// at or above diskUsedFailPercent. -P guarantees one line per filesystem and a
// stable "Capacity" column, so the used% is the field ending in '%'.
func evalDiskHeadroom(res NodeExecResult) (bool, string) {
	pct, ok := parseDfUsedPercent(res.Stdout)
	if !ok {
		return false, "could not read df output"
	}
	detail := itoaNonNeg(pct) + "% used"
	if pct >= diskUsedFailPercent {
		return false, detail
	}
	return true, detail
}

// parseDfUsedPercent pulls the used-capacity percentage out of `df -P` output.
// It scans every field of the last non-header line for the token ending in '%'
// so it does not depend on a fixed column index.
func parseDfUsedPercent(out string) (int, bool) {
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) == 0 {
		return 0, false
	}
	last := lines[len(lines)-1]
	for _, f := range strings.Fields(last) {
		if !strings.HasSuffix(f, "%") {
			continue
		}
		n, ok := atoiNonNeg(strings.TrimSuffix(f, "%"))
		if ok {
			return n, true
		}
	}
	return 0, false
}

// firstField returns the first whitespace-delimited token, or the trimmed input
// when there is none.
func firstField(s string) string {
	if f := strings.Fields(s); len(f) > 0 {
		return f[0]
	}
	return strings.TrimSpace(s)
}

// versionTriple renders a parsed version back to "vX.Y.Z" for display.
func versionTriple(v [3]int) string {
	return "v" + itoaNonNeg(v[0]) + "." + itoaNonNeg(v[1]) + "." + itoaNonNeg(v[2])
}

// atoiNonNeg parses a non-negative decimal integer without pulling in strconv's
// error type; ok is false on any non-digit.
func atoiNonNeg(s string) (int, bool) {
	if s == "" {
		return 0, false
	}
	n := 0
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return 0, false
		}
		n = n*10 + int(s[i]-'0')
	}
	return n, true
}

// itoaNonNeg formats a non-negative int (exit codes and percentages).
func itoaNonNeg(i int) string {
	if i == 0 {
		return "0"
	}
	neg := i < 0
	if neg {
		i = -i
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	if neg {
		b = append([]byte{'-'}, b...)
	}
	return string(b)
}
