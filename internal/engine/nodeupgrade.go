package engine

// ephemerd upgrade strategy for `mayfly node upgrade`.
//
// ephemerd v0.1.7 grew `ephemerd upgrade --version <tag>`: the daemon fetches
// its own release asset over HTTPS and restarts itself. That is the daemon's
// own supported path, it is identical on every OS, and — the reason it matters
// here — it needs no hypervisor file channel at all, so it works the same on a
// Proxmox VM and an SSH-enrolled Mac. Whenever a node reports a version that
// has it, mayfly uses it.
//
// Older daemons keep the historical push: download the release asset in-guest,
// replace the binary, drain, restart the service. That path is OS-specific (a
// tarball + systemd on linux, a tarball + launchd on darwin, a zip + a Windows
// service), and it is the only reason an OS switch survives in this file.
//
// A daemon that reports NO version — `--version` predates some very old builds,
// and a wedged daemon answers nothing at all — is treated as old and pushed to.
// The push path works on every version, so guessing "old" is the safe guess;
// guessing "new" would run a subcommand that does not exist and leave the node
// on its old binary while reporting success.

import (
	"errors"
	"fmt"
	"regexp"

	"github.com/ephemerd/mayfly/internal/config"
)

// unixExe is where ephemerd installs on linux and darwin. Always absolute:
// /usr/local/bin is not on a non-interactive PATH on macOS, so a bare
// `ephemerd` fails over SSH even though it works in the operator's own shell.
const unixExe = "/usr/local/bin/ephemerd"

// selfUpgradeMinVersion is the first ephemerd release with `ephemerd upgrade`.
var selfUpgradeMinVersion = [3]int{0, 1, 7}

// EphemerdArgv builds an ephemerd CLI invocation for a node's OS.
func EphemerdArgv(osName string, args ...string) []string {
	exe := unixExe
	if osName == "windows" {
		exe = winExe
	}
	return append([]string{exe}, args...)
}

// EphemerdVersionArgv asks a node's ephemerd to identify itself.
func EphemerdVersionArgv(osName string) []string {
	return EphemerdArgv(osName, "--version")
}

// versionRe finds a semantic version anywhere in a daemon's --version output,
// which has varied across releases ("ephemerd v0.1.8", "ephemerd version
// 0.1.8 (abc1234)", a bare "v0.1.8").
var versionRe = regexp.MustCompile(`v?(\d+)\.(\d+)\.(\d+)`)

// parseVersion extracts the first semantic version in s. ok is false when there
// is none — an old daemon that rejected --version, a dev build, or empty output
// from a daemon that never answered.
func parseVersion(s string) ([3]int, bool) {
	m := versionRe.FindStringSubmatch(s)
	if m == nil {
		return [3]int{}, false
	}
	var v [3]int
	for i := range v {
		if _, err := fmt.Sscanf(m[i+1], "%d", &v[i]); err != nil {
			return [3]int{}, false
		}
	}
	return v, true
}

// supportsSelfUpgrade reports whether a node's reported --version output is at
// least the release that added `ephemerd upgrade`.
func supportsSelfUpgrade(reported string) bool {
	v, ok := parseVersion(reported)
	if !ok {
		return false
	}
	for i := range v {
		if v[i] != selfUpgradeMinVersion[i] {
			return v[i] > selfUpgradeMinVersion[i]
		}
	}
	return true
}

// NodeUpgradeMethod is how `mayfly node upgrade` moves a node.
type NodeUpgradeMethod string

const (
	// UpgradeSelf invokes the daemon's own `ephemerd upgrade --version`.
	UpgradeSelf NodeUpgradeMethod = "self"
	// UpgradePush downloads and installs the release asset from a shell script
	// run on the node — the pre-v0.1.7 path.
	UpgradePush NodeUpgradeMethod = "push"
)

// NodeUpgrade is the chosen upgrade for one node.
type NodeUpgrade struct {
	Method NodeUpgradeMethod
	// Argv is the command to run on the node, ready for a NodeExecutor.
	Argv []string
	// Version is the release tag being installed (empty when binary_url pins a
	// specific artifact instead), and URL the asset the push path downloads
	// (empty on the self path).
	Version string
	URL     string
}

// PlanNodeUpgrade picks the upgrade method for a node running reportedVersion
// and builds the command that performs it. Pure — it neither dials nor runs
// anything.
//
// An explicit ephemerd.binary_url forces the push path: it pins one specific
// artifact (a local build, a patched release), and `ephemerd upgrade` only
// takes a release TAG, so honoring the pin means installing it by hand.
func PlanNodeUpgrade(src config.Ephemerd, osName, arch, reportedVersion string) (NodeUpgrade, error) {
	if src.Version == "" && src.BinaryURL == "" {
		return NodeUpgrade{}, errors.New("ephemerd.version or ephemerd.binary_url must be set")
	}
	if src.BinaryURL == "" && supportsSelfUpgrade(reportedVersion) {
		return NodeUpgrade{
			Method:  UpgradeSelf,
			Version: src.Version,
			Argv:    EphemerdArgv(osName, "upgrade", "--version", src.Version),
		}, nil
	}
	url, err := resolveEphemerdURL(src, osName, arch)
	if err != nil {
		return NodeUpgrade{}, err
	}
	argv, err := pushUpgradeArgv(osName, url)
	if err != nil {
		return NodeUpgrade{}, err
	}
	// A binary_url pin names an ARTIFACT, not a release tag, and the two can
	// disagree: the pinned build may report anything (a dev stamp like
	// v0.1.10-5-gda802c9, or the version it was forked from). Reporting
	// src.Version as the installed version would be a guess, and callers that
	// verify the upgrade afterwards would compare the node's honest answer
	// against that guess and declare a successful install a failure. Empty
	// means "no tag to assert" — verify that it came back, not what it says.
	version := src.Version
	if src.BinaryURL != "" {
		version = ""
	}
	return NodeUpgrade{Method: UpgradePush, Version: version, URL: url, Argv: argv}, nil
}

// pushUpgradeArgv builds the in-guest script that installs url and bounces the
// service, for daemons without a self-upgrade.
func pushUpgradeArgv(osName, url string) ([]string, error) {
	switch osName {
	case "windows":
		// Every statement that can fail is checked, and the restart is in a
		// `finally`. The previous version of this script was a bare `;`-chain
		// with PowerShell's default non-terminating errors, which failed in the
		// worst possible way on 2026-09-21: it drained the node, stopped the
		// service, and then did not replace the binary — leaving the box down,
		// on the OLD version, for 25 minutes until someone looked. A node that
		// is merely un-upgraded still runs CI; a node that is stopped does not.
		// So the invariant here is: come back up no matter what. If the swap
		// fails we restart the old binary and report the failure, rather than
		// exiting with the service stopped.
		//
		// The swap is Move-Item, not Copy-Item, and that is load-bearing. The
		// Windows asset unpacks to a ~1.1 GB executable; copying it takes real
		// time, and Defender rescans the whole thing on write. Doing that
		// BETWEEN Stop-Service and Start-Service put a multi-minute,
		// AV-dependent stall inside the window where the node is down — long
		// enough to blow the caller's verification deadline. TEMP and the
		// install dir are both on C:, so a move is a rename: constant-time, and
		// the service-down window is now just the service bounce.
		//
		// The old binary is kept as .old (same convention as ephemerd's own
		// self-upgrade) so a bad release can be rolled back by hand.
		//
		// THE STOP WAIT IS 8 MINUTES AND IT IS SWALLOWED, for two separate
		// reasons that both cost a node.
		//
		// Duration: ephemerd allows a drain up to 5 minutes. The first version
		// of this script waited 2, which is fine on an idle node and wrong on a
		// busy one — the same mistake #44 fixed in the config-tier restart.
		//
		// try/catch: WaitForStatus THROWS on timeout, and under
		// $ErrorActionPreference='Stop' that aborts the script — before the
		// try/finally below exists, so the guaranteed Start-Service never runs.
		// That is precisely the outcome this script's `finally` was added to
		// prevent, reintroduced two lines above it. Rolling v0.2.16 on
		// 2026-09-22 left mfl-win-amd64-102 stopped on the old binary with the
		// new one fully staged: download fine, extract fine, verified fine,
		// node dead.
		//
		// Swallowing the timeout is correct rather than lazy. If the service
		// is somehow still running, the Move-Item fails, the catch rolls the
		// old binary back, and the finally restarts it — a worse outcome than
		// upgrading, but a LIVE node, which is the property that matters.
		script := fmt.Sprintf(
			`$ErrorActionPreference='Stop'; $ProgressPreference='SilentlyContinue'; `+
				`$exe='%s'; $stage="$env:TEMP\ephemerd-up"; `+
				`Invoke-WebRequest -UseBasicParsing -Uri %q -OutFile "$env:TEMP\ephemerd.zip"; `+
				`Remove-Item $stage -Recurse -Force -ErrorAction SilentlyContinue; `+
				`Expand-Archive -Path "$env:TEMP\ephemerd.zip" -DestinationPath $stage -Force; `+
				`if (-not (Test-Path "$stage\ephemerd.exe")) { throw "archive contained no ephemerd.exe" }; `+
				`& $exe drain; `+
				`Stop-Service ephemerd; `+
				`try { (Get-Service ephemerd).WaitForStatus('Stopped','00:08:00') } catch {}; `+
				`try { `+
				`Remove-Item "$exe.old" -Force -ErrorAction SilentlyContinue; `+
				`Move-Item $exe "$exe.old" -Force; `+
				`Move-Item "$stage\ephemerd.exe" $exe -Force `+
				`} catch { Move-Item "$exe.old" $exe -Force -ErrorAction SilentlyContinue; throw } `+
				`finally { Start-Service ephemerd }; `+
				`Remove-Item "$env:TEMP\ephemerd.zip",$stage -Recurse -Force -ErrorAction SilentlyContinue`,
			winExe, url)
		return []string{"powershell", "-NoProfile", "-Command", script}, nil
	case "linux", "darwin":
		// The drain is best-effort (`|| true`): a daemon that is already down
		// must not block its own replacement. Everything before it is fatal
		// under set -e, so a failed download never half-installs.
		// enrollServiceStart supplies the platform's restart — systemd on
		// linux, ephemerd's own launchd subcommands on darwin — the same
		// fragment the enrollment path uses, so an upgraded box and a freshly
		// enrolled one end up in the same service state.
		script := fmt.Sprintf(
			"set -e\n"+
				"curl -fsSL -o /tmp/ephemerd.tar.gz %q\n"+
				"tar -C /usr/local/bin -xzf /tmp/ephemerd.tar.gz ephemerd\n"+
				"chmod 0755 /usr/local/bin/ephemerd\n"+
				"rm -f /tmp/ephemerd.tar.gz\n"+
				"%s drain || true\n"+
				"%s\n",
			url, unixExe, enrollServiceStart(osName))
		return []string{"/bin/sh", "-c", script}, nil
	default:
		return nil, fmt.Errorf("no upgrade path for os %q", osName)
	}
}
