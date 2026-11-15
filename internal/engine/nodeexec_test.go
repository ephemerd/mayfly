package engine

import (
	"errors"
	"os/exec"
	"runtime"
	"testing"

	"github.com/luthermonson/mayfly/internal/config"
	"github.com/luthermonson/mayfly/internal/state"
)

// fleetConfig mirrors the live fleet: an amd64 Proxmox pool, an arm64 Proxmox
// pool on a dedicated node, a darwin pool enrolled over SSH, and a windows
// Proxmox pool.
func fleetConfig() *config.Config {
	return &config.Config{
		Proxmox: config.Proxmox{URL: "https://pve:8006", TokenID: "t", Secret: "s", Node: "node-a", Storage: "local"},
		Pools: map[string]config.PoolSpec{
			"linux-amd64": {Hostname: "ci-linux-amd64", Size: 1},
			"linux-arm64": {Hostname: "ci-linux-arm64", Size: 1, Arch: "arm64", Node: "node-b"},
			"mac-arm64": {
				Hostname: "ci-mac-arm64", OS: "darwin",
				Members: []string{"192.0.2.78"}, SSHUser: "ephemerd", SSHKey: "~/.ssh/ephemerd",
			},
			"win-amd64": {Hostname: "ci-win-amd64", OS: "windows", Size: 1, Node: "node-c"},
		},
	}
}

// ResolveNodeTarget must route each pool to the channel it actually speaks:
// the Proxmox guest agent for VM pools, SSH for enrolled bare metal. Getting
// this wrong is the original bug: an enrolled Mac (id 0) had nothing to look up
// on a hypervisor at all.
func TestResolveNodeTargetRoutesByProvider(t *testing.T) {
	cfg := fleetConfig()
	tests := []struct {
		name      string
		pool      *state.Pool
		member    *state.Member
		transport NodeTransport
		provider  config.Provider
		node      string
		id        int
	}{
		{
			name:      "proxmox pool uses the guest agent",
			pool:      &state.Pool{Name: "linux-amd64"},
			member:    &state.Member{VMID: 100, Hostname: "mfl-linux-amd64-100", PVENode: "node-a", OS: "linux", Arch: "amd64"},
			transport: NodeTransportHypervisor, provider: config.ProviderProxmox, node: "node-a", id: 100,
		},
		{
			name:      "arm64 pool is a proxmox VM on its own node",
			pool:      &state.Pool{Name: "linux-arm64"},
			member:    &state.Member{VMID: 200, Hostname: "mfl-linux-arm64-200", PVENode: "node-b", OS: "linux", Arch: "arm64"},
			transport: NodeTransportHypervisor, provider: config.ProviderProxmox, node: "node-b", id: 200,
		},
		{
			name:      "windows pool is still proxmox",
			pool:      &state.Pool{Name: "win-amd64"},
			member:    &state.Member{VMID: 101, Hostname: "mfl-win-amd64-101", PVENode: "node-c", OS: "windows", Arch: "amd64"},
			transport: NodeTransportHypervisor, provider: config.ProviderProxmox, node: "node-c", id: 101,
		},
		{
			name:      "darwin pool is ssh bare metal",
			pool:      &state.Pool{Name: "mac-arm64", SSHUser: "ephemerd", SSHKey: "~/.ssh/ephemerd"},
			member:    &state.Member{Hostname: "192.0.2.78", OS: "darwin", Arch: "arm64"},
			transport: NodeTransportSSH,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := ResolveNodeTarget(cfg, tc.pool, tc.member)
			if got.Transport != tc.transport {
				t.Fatalf("Transport = %q, want %q", got.Transport, tc.transport)
			}
			if got.Transport != NodeTransportHypervisor {
				return
			}
			if got.Provider != tc.provider {
				t.Errorf("Provider = %q, want %q", got.Provider, tc.provider)
			}
			if got.Node != tc.node {
				t.Errorf("Node = %q, want %q", got.Node, tc.node)
			}
			if got.ID != tc.id {
				t.Errorf("ID = %d, want %d", got.ID, tc.id)
			}
		})
	}
}

// A linux pool that enrolls existing bare metal has no hypervisor either, even
// though linux pools normally do.
func TestResolveNodeTargetEnrolledLinuxIsSSH(t *testing.T) {
	cfg := &config.Config{
		Pools: map[string]config.PoolSpec{
			"linux-bare": {Hostname: "h", Members: []string{"10.0.0.9"}, SSHUser: "root"},
		},
	}
	pool := &state.Pool{Name: "linux-bare", SSHUser: "root"}
	got := ResolveNodeTarget(cfg, pool, &state.Member{Hostname: "10.0.0.9", OS: "linux", Arch: "amd64"})
	if got.Transport != NodeTransportSSH {
		t.Fatalf("Transport = %q, want ssh", got.Transport)
	}
	if got.SSH.Host != "10.0.0.9" || got.SSH.User != "root" {
		t.Errorf("SSH = %+v, want host 10.0.0.9 user root", got.SSH)
	}
}

// A node whose pool has left the config still has to be reachable — that is
// precisely when an operator is trying to clean up by hand. Fall back to state:
// no recorded backend id means bare metal, otherwise Proxmox (the historical
// default, matching Engine.providerForPool).
func TestResolveNodeTargetPoolMissingFromConfig(t *testing.T) {
	cfg := &config.Config{Pools: map[string]config.PoolSpec{}}

	vm := ResolveNodeTarget(cfg, &state.Pool{Name: "gone"},
		&state.Member{VMID: 100, PVENode: "node-a", Hostname: "mfl-gone-100", OS: "linux"})
	if vm.Transport != NodeTransportHypervisor || vm.Provider != config.ProviderProxmox {
		t.Errorf("vm member: got %q/%q, want hypervisor/proxmox", vm.Transport, vm.Provider)
	}

	bare := ResolveNodeTarget(cfg, &state.Pool{Name: "gone", SSHUser: "ephemerd"},
		&state.Member{Hostname: "192.0.2.78", OS: "darwin"})
	if bare.Transport != NodeTransportSSH {
		t.Errorf("bare member: got %q, want ssh", bare.Transport)
	}
}

// A member with no backend id is bare metal no matter what the pool config
// says: mayfly never allocates id 0 for a VM, so there is nothing to look up on
// a hypervisor. Guards against a mis-tagged pool sending an SSH node to Proxmox.
func TestResolveNodeTargetZeroIDAlwaysSSH(t *testing.T) {
	cfg := fleetConfig()
	got := ResolveNodeTarget(cfg, &state.Pool{Name: "linux-arm64", SSHUser: "root"},
		&state.Member{Hostname: "10.0.0.5", OS: "linux", Arch: "arm64"})
	if got.Transport != NodeTransportSSH {
		t.Fatalf("Transport = %q, want ssh (vmid 0 is the enrollment signature)", got.Transport)
	}
}

// State is authoritative for SSH credentials (apply converges them there), but
// a state file written before those fields existed must still fall back to the
// config spec rather than failing to connect.
func TestNodeSSHTargetPrefersStateThenConfig(t *testing.T) {
	spec := config.PoolSpec{SSHUser: "cfguser", SSHKey: "cfgkey", SSHPort: 2222, SSHPassword: "cfgpw"}

	fromState := nodeSSHTarget(
		&state.Pool{SSHUser: "stuser", SSHKey: "stkey", SSHPort: 2022, SSHPassword: "stpw"}, spec, "h")
	want := SSHTarget{User: "stuser", Host: "h", Port: 2022, KeyPath: "stkey", Password: "stpw"}
	if fromState != want {
		t.Errorf("state-populated pool: got %+v, want %+v", fromState, want)
	}

	fromCfg := nodeSSHTarget(&state.Pool{}, spec, "h")
	wantCfg := SSHTarget{User: "cfguser", Host: "h", Port: 2222, KeyPath: "cfgkey", Password: "cfgpw"}
	if fromCfg != wantCfg {
		t.Errorf("empty state pool: got %+v, want %+v", fromCfg, wantCfg)
	}
}

// NewNodeExecutor builds a hypervisor executor for a hypervisor target and an
// SSH executor for an SSH target.
func TestNewNodeExecutorBuildsTheNamedBackend(t *testing.T) {
	cfg := &config.Config{
		Proxmox: config.Proxmox{URL: "https://pve:8006", TokenID: "t", Secret: "s", Node: "n", Storage: "st"},
	}

	ex, err := NewNodeExecutor(cfg, NodeTarget{
		Transport: NodeTransportHypervisor, Provider: config.ProviderProxmox, Node: "n", ID: 100, Name: "vm",
	})
	if err != nil {
		t.Fatalf("proxmox target: %v, want nil", err)
	}
	if _, ok := ex.(*hypervisorExecutor); !ok {
		t.Errorf("proxmox target gave %T, want *hypervisorExecutor", ex)
	}

	ssh, err := NewNodeExecutor(cfg, NodeTarget{
		Transport: NodeTransportSSH, Name: "mac",
		SSH: SSHTarget{User: "ephemerd", Host: "192.0.2.78"},
	})
	if err != nil {
		t.Fatalf("ssh target: %v, want nil", err)
	}
	if _, ok := ssh.(*sshExecutor); !ok {
		t.Errorf("ssh target gave %T, want *sshExecutor", ssh)
	}
}

// An SSH target with nothing to dial must say so, not fail later inside ssh.
func TestNewNodeExecutorRejectsIncompleteSSHTarget(t *testing.T) {
	cfg := &config.Config{}
	for _, tc := range []struct {
		name   string
		target SSHTarget
		want   string
	}{
		{"no host", SSHTarget{User: "u"}, "no ssh host"},
		{"no user", SSHTarget{Host: "h"}, "no ssh_user"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NewNodeExecutor(cfg, NodeTarget{Transport: NodeTransportSSH, Name: "n", SSH: tc.target})
			if err == nil {
				t.Fatalf("got nil error, want one mentioning %q", tc.want)
			}
		})
	}
}

func TestShellJoin(t *testing.T) {
	tests := []struct {
		argv []string
		want string
	}{
		{[]string{"/usr/local/bin/ephemerd", "status"}, `'/usr/local/bin/ephemerd' 'status'`},
		{[]string{"echo", "a b"}, `'echo' 'a b'`},
		// A single quote in an argument must not end the quoting and let the
		// rest run as shell.
		{[]string{"echo", `it's; rm -rf /`}, `'echo' 'it'\''s; rm -rf /'`},
		{nil, ""},
	}
	for _, tc := range tests {
		if got := ShellJoin(tc.argv); got != tc.want {
			t.Errorf("ShellJoin(%q) = %q, want %q", tc.argv, got, tc.want)
		}
	}
}

// remoteExitCode must recover a remote command's exit status, while refusing to
// pass off ssh's own 255 (unreachable host, auth failure) as a command result.
func TestRemoteExitCode(t *testing.T) {
	if got, ok := remoteExitCode(errors.New("dial tcp: connection refused")); ok {
		t.Errorf("plain error: got (%d, true), want (_, false)", got)
	}

	code, ok := remoteExitCode(wrapExit(t, 7))
	if !ok || code != 7 {
		t.Errorf("exit 7: got (%d, %v), want (7, true)", code, ok)
	}

	if _, ok := remoteExitCode(wrapExit(t, sshSelfFailureCode)); ok {
		t.Error("exit 255 is ssh's own failure code and must not read as a command exit")
	}
}

// wrapExit produces a wrapped *exec.ExitError with the given status, the way
// SSHRun wraps the failure of the ssh binary.
func wrapExit(t *testing.T, code int) error {
	t.Helper()
	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		cmd = exec.Command("cmd", "/c", "exit", itoa(code))
	} else {
		cmd = exec.Command("sh", "-c", "exit "+itoa(code))
	}
	err := cmd.Run()
	if err == nil {
		t.Fatalf("helper command exited 0, want %d", code)
	}
	var ee *exec.ExitError
	if !errors.As(err, &ee) {
		t.Fatalf("helper command failed without an ExitError: %v", err)
	}
	return errWrap{err}
}

type errWrap struct{ err error }

func (e errWrap) Error() string { return "ssh u@h: " + e.err.Error() }
func (e errWrap) Unwrap() error { return e.err }

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	return string(b)
}
