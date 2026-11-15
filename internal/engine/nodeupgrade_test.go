package engine

import (
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/luthermonson/mayfly/internal/config"
)

// ephemerd lives at an absolute path on every platform: /usr/local/bin is not
// on a non-interactive PATH on macOS, so a bare `ephemerd` fails over SSH.
func TestEphemerdArgv(t *testing.T) {
	tests := []struct {
		osName string
		args   []string
		want   []string
	}{
		{"linux", []string{"status"}, []string{"/usr/local/bin/ephemerd", "status"}},
		{"darwin", []string{"status"}, []string{"/usr/local/bin/ephemerd", "status"}},
		{"windows", []string{"status"}, []string{winExe, "status"}},
		{"linux", []string{"jobs", "list"}, []string{"/usr/local/bin/ephemerd", "jobs", "list"}},
	}
	for _, tc := range tests {
		got := EphemerdArgv(tc.osName, tc.args...)
		if strings.Join(got, "\x00") != strings.Join(tc.want, "\x00") {
			t.Errorf("EphemerdArgv(%q, %q) = %q, want %q", tc.osName, tc.args, got, tc.want)
		}
	}
}

func TestParseVersion(t *testing.T) {
	tests := []struct {
		in   string
		want [3]int
		ok   bool
	}{
		{"ephemerd v0.1.8", [3]int{0, 1, 8}, true},
		{"ephemerd version 0.1.7 (abc1234)", [3]int{0, 1, 7}, true},
		{"v1.2.30\n", [3]int{1, 2, 30}, true},
		{"", [3]int{}, false},
		{"dev", [3]int{}, false},
		// An old daemon rejects the flag rather than answering it.
		{"Error: unknown flag: --version", [3]int{}, false},
	}
	for _, tc := range tests {
		got, ok := parseVersion(tc.in)
		if ok != tc.ok || got != tc.want {
			t.Errorf("parseVersion(%q) = (%v, %v), want (%v, %v)", tc.in, got, ok, tc.want, tc.ok)
		}
	}
}

// v0.1.7 is the first release with `ephemerd upgrade`. A daemon that cannot say
// what it is must read as OLD: the push path works on every version, whereas
// guessing "new" runs a subcommand that does not exist.
func TestSupportsSelfUpgrade(t *testing.T) {
	tests := []struct {
		in   string
		want bool
	}{
		{"ephemerd v0.1.7", true},
		{"ephemerd v0.1.8", true},
		{"ephemerd v0.2.0", true},
		{"ephemerd v1.0.0", true},
		{"ephemerd v0.1.6", false},
		{"ephemerd v0.0.9", false},
		{"", false},
		{"dev", false},
	}
	for _, tc := range tests {
		if got := supportsSelfUpgrade(tc.in); got != tc.want {
			t.Errorf("supportsSelfUpgrade(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

// A modern daemon upgrades itself over HTTPS. That command is identical on
// every transport — it is what makes `node upgrade` work on the SSH-enrolled
// Mac without any hypervisor file channel.
func TestPlanNodeUpgradeSelfPath(t *testing.T) {
	src := config.Ephemerd{Version: "v0.1.8"}
	for _, osName := range []string{"linux", "darwin", "windows"} {
		up, err := PlanNodeUpgrade(src, osName, "arm64", "ephemerd v0.1.7")
		if err != nil {
			t.Fatalf("%s: %v", osName, err)
		}
		if up.Method != UpgradeSelf {
			t.Errorf("%s: Method = %q, want %q", osName, up.Method, UpgradeSelf)
		}
		want := EphemerdArgv(osName, "upgrade", "--version", "v0.1.8")
		if strings.Join(up.Argv, " ") != strings.Join(want, " ") {
			t.Errorf("%s: Argv = %q, want %q", osName, up.Argv, want)
		}
		if up.URL != "" {
			t.Errorf("%s: URL = %q, want empty (self-upgrade downloads its own asset)", osName, up.URL)
		}
	}
}

// Pre-v0.1.7 daemons, and daemons that report nothing, get the historical push
// — with the release asset for the NODE's os/arch, not mayfly's.
func TestPlanNodeUpgradePushPath(t *testing.T) {
	src := config.Ephemerd{Version: "v0.1.8"}
	tests := []struct {
		osName, arch string
		reported     string
		wantURL      string
		wantShell    string
	}{
		{"linux", "amd64", "ephemerd v0.1.6",
			"https://github.com/ephpm/ephemerd/releases/download/v0.1.8/ephemerd_v0.1.8_linux_amd64.tar.gz", "/bin/sh"},
		{"linux", "arm64", "",
			"https://github.com/ephpm/ephemerd/releases/download/v0.1.8/ephemerd_v0.1.8_linux_arm64.tar.gz", "/bin/sh"},
		{"darwin", "arm64", "",
			"https://github.com/ephpm/ephemerd/releases/download/v0.1.8/ephemerd_v0.1.8_darwin_arm64.tar.gz", "/bin/sh"},
		{"windows", "amd64", "ephemerd v0.1.5",
			"https://github.com/ephpm/ephemerd/releases/download/v0.1.8/ephemerd_v0.1.8_windows_amd64.zip", "powershell"},
	}
	for _, tc := range tests {
		up, err := PlanNodeUpgrade(src, tc.osName, tc.arch, tc.reported)
		if err != nil {
			t.Fatalf("%s/%s: %v", tc.osName, tc.arch, err)
		}
		if up.Method != UpgradePush {
			t.Errorf("%s/%s: Method = %q, want %q", tc.osName, tc.arch, up.Method, UpgradePush)
		}
		if up.URL != tc.wantURL {
			t.Errorf("%s/%s: URL = %q, want %q", tc.osName, tc.arch, up.URL, tc.wantURL)
		}
		if up.Argv[0] != tc.wantShell {
			t.Errorf("%s/%s: Argv[0] = %q, want %q", tc.osName, tc.arch, up.Argv[0], tc.wantShell)
		}
		if !strings.Contains(strings.Join(up.Argv, " "), tc.wantURL) {
			t.Errorf("%s/%s: script does not reference the asset URL", tc.osName, tc.arch)
		}
	}
}

// The push script must bounce the service the way that platform manages it —
// systemd on linux, ephemerd's own launchd subcommands on darwin. A darwin node
// handed `systemctl restart` would install the new binary and never run it.
func TestPushUpgradeArgvRestartsPerPlatform(t *testing.T) {
	linux, err := pushUpgradeArgv("linux", "https://example/x.tar.gz")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(linux[2], "systemctl restart ephemerd") {
		t.Errorf("linux script does not restart via systemd:\n%s", linux[2])
	}

	dar, err := pushUpgradeArgv("darwin", "https://example/x.tar.gz")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(dar[2], "systemctl") {
		t.Errorf("darwin script uses systemctl, which macOS does not have:\n%s", dar[2])
	}
	if !strings.Contains(dar[2], unixExe+" restart") {
		t.Errorf("darwin script does not restart via ephemerd:\n%s", dar[2])
	}

	if _, err := pushUpgradeArgv("plan9", "https://example/x.tar.gz"); err == nil {
		t.Error("pushUpgradeArgv(plan9) = nil error, want an unsupported-os error")
	}
}

// Regression: on 2026-09-21 the Windows push drained the node, stopped the
// service, failed to replace the binary, and exited — leaving the box DOWN on
// the old version. Two properties keep that from recurring, and both are easy
// to undo by accident while editing a long format string, so both are pinned.
func TestPushUpgradeArgvWindowsCannotLeaveNodeStopped(t *testing.T) {
	win, err := pushUpgradeArgv("windows", "https://example/x.zip")
	if err != nil {
		t.Fatal(err)
	}
	script := win[len(win)-1]

	// 1. Errors must terminate. Under PowerShell's default non-terminating
	// errors a failed swap falls through to the next statement, so the script
	// reports success having installed nothing.
	if !strings.Contains(script, "$ErrorActionPreference='Stop'") {
		t.Errorf("windows script does not make errors terminating:\n%s", script)
	}

	// 2. The restart must be unconditional. Anywhere else and a failure between
	// Stop-Service and Start-Service exits with the node stopped — strictly
	// worse than not upgrading at all, since an un-upgraded node still runs CI.
	if !strings.Contains(script, "finally { Start-Service ephemerd }") {
		t.Errorf("windows script does not restart the service unconditionally:\n%s", script)
	}

	// The ~1.1 GB binary must be MOVED into place (a rename within C:), not
	// copied. A copy inside the service-down window is a multi-minute,
	// Defender-dependent stall that blows the caller's verification deadline.
	if strings.Contains(script, "Copy-Item") {
		t.Errorf("windows script copies the binary instead of moving it:\n%s", script)
	}

	// 3. Nothing may throw BEFORE the try/finally is entered. WaitForStatus
	// throws on timeout, and under $ErrorActionPreference='Stop' that aborts
	// the script while the service is down — defeating the `finally` from two
	// lines away. Rolling v0.2.16 did exactly that: binary staged and verified,
	// node left stopped on the old version.
	guarded := regexp.MustCompile(`try \{ \(Get-Service ephemerd\)\.WaitForStatus\('Stopped',[^)]*\) \} catch \{\}`)
	if !guarded.MatchString(script) {
		t.Errorf("the Stopped wait is not wrapped in try/catch; a timeout aborts before the finally:\n%s", script)
	}
}

// The stop wait in the UPGRADE path has to outlast a drain for the same reason
// the restart path's does (see TestRestartCmdWindowsWaitsOutTheDrain). These
// are two separate scripts and fixing one silently leaves the other broken —
// which is what happened: #43 fixed the restart shape here but kept a 2-minute
// wait, and the next upgrade with a slow drain killed the node anyway.
func TestPushUpgradeArgvWindowsStopWaitOutlastsDrain(t *testing.T) {
	win, err := pushUpgradeArgv("windows", "https://example/x.zip")
	if err != nil {
		t.Fatal(err)
	}
	script := win[len(win)-1]

	m := regexp.MustCompile(`WaitForStatus\('Stopped','(\d+):(\d+):(\d+)'\)`).FindStringSubmatch(script)
	if m == nil {
		t.Fatalf("no WaitForStatus('Stopped',...) in the windows upgrade:\n%s", script)
	}
	h, _ := strconv.Atoi(m[1])
	min, _ := strconv.Atoi(m[2])
	sec, _ := strconv.Atoi(m[3])
	got := time.Duration(h)*time.Hour + time.Duration(min)*time.Minute + time.Duration(sec)*time.Second

	const ephemerdDrainCeiling = 5 * time.Minute
	if got <= ephemerdDrainCeiling {
		t.Errorf("upgrade stop wait is %s, which does not outlast ephemerd's %s drain", got, ephemerdDrainCeiling)
	}
}

// An explicit binary_url pins one artifact. `ephemerd upgrade` only takes a
// release TAG, so honoring the pin means pushing even to a daemon that could
// upgrade itself — otherwise mayfly would silently install a different build
// than the operator asked for.
func TestPlanNodeUpgradeBinaryURLForcesPush(t *testing.T) {
	src := config.Ephemerd{Version: "v0.1.8", BinaryURL: "https://example/custom.tar.gz"}
	up, err := PlanNodeUpgrade(src, "linux", "amd64", "ephemerd v0.1.8")
	if err != nil {
		t.Fatal(err)
	}
	if up.Method != UpgradePush {
		t.Fatalf("Method = %q, want %q", up.Method, UpgradePush)
	}
	if up.URL != "https://example/custom.tar.gz" {
		t.Errorf("URL = %q, want the pinned artifact", up.URL)
	}
	// A pinned artifact is not a release tag, and the build it contains may
	// report anything at all (a dev stamp, the version it forked from). The
	// tag alongside it is what the push path IGNORES, so carrying it here as
	// "the version installed" is a claim nothing checked — until callers
	// started verifying the upgrade against the node's own answer, at which
	// point a successful install gets declared a failure for disagreeing with
	// a guess.
	if up.Version != "" {
		t.Errorf("Version = %q, want empty: binary_url pins an artifact, not a tag", up.Version)
	}
}

func TestPlanNodeUpgradeRequiresAVersion(t *testing.T) {
	if _, err := PlanNodeUpgrade(config.Ephemerd{}, "linux", "amd64", "ephemerd v0.1.8"); err == nil {
		t.Error("got nil error, want a complaint that no version or binary_url is set")
	}
}

// A pool's ephemerd_version pin must steer node upgrade the same way it steers
// apply — canarying a release on one pool should not be undone by an operator
// running `mayfly node upgrade`.
func TestEphemerdSourceHonorsPoolPin(t *testing.T) {
	global := config.Ephemerd{Version: "v0.1.8"}
	pinned := EphemerdSource(global, config.PoolSpec{EphemerdVersion: "v0.1.9"})
	if pinned.Version != "v0.1.9" {
		t.Errorf("Version = %q, want the pool pin v0.1.9", pinned.Version)
	}
	unpinned := EphemerdSource(global, config.PoolSpec{})
	if unpinned.Version != "v0.1.8" {
		t.Errorf("Version = %q, want the global v0.1.8", unpinned.Version)
	}
}
