package engine

import (
	"bytes"
	"context"
	"errors"
	"os/exec"
	"reflect"
	"strings"
	"testing"

	"github.com/luthermonson/mayfly/internal/provider"
	"github.com/luthermonson/mayfly/internal/state"
)

// scriptInstance records file-writes in order and answers the script's run
// exec with a canned status. Bookkeeping execs (join, remove) succeed.
type scriptInstance struct {
	*captureInstance
	order   []string
	run     provider.ExecStatus
	runErr  error
	runArgv []string
}

func newScriptInstance() *scriptInstance {
	return &scriptInstance{captureInstance: newCaptureInstance(100)}
}

func (s *scriptInstance) FileWrite(ctx context.Context, path string, content []byte) error {
	s.order = append(s.order, path)
	return s.captureInstance.FileWrite(ctx, path, content)
}

func (s *scriptInstance) Exec(ctx context.Context, argv []string, timeoutSec int) (provider.ExecStatus, error) {
	_, _ = s.captureInstance.Exec(ctx, argv, timeoutSec)
	if isBookkeeping(argv) {
		return provider.ExecStatus{}, nil
	}
	s.runArgv = argv
	return s.run, s.runErr
}

// isBookkeeping spots the join/remove execs, which are not the script itself.
func isBookkeeping(argv []string) bool {
	j := strings.Join(argv, " ")
	return argv[0] == "rm" || strings.Contains(j, " del ") || strings.Contains(j, " copy /b ") ||
		strings.Contains(j, `cat -- "$@"`)
}

func pinToken(t *testing.T, tok string) {
	t.Helper()
	prev := scriptToken
	scriptToken = func() string { return tok }
	t.Cleanup(func() { scriptToken = prev })
}

func scriptExecutor(osName string, inst provider.Instance) *hypervisorExecutor {
	return &hypervisorExecutor{name: "mfl-test-100", os: osName, inst: inst}
}

// The quote soup that broke the PowerShell parser on the operator's side. It
// must arrive in the guest byte for byte.
const quoteSoup = `uname -a; echo "quote test: \"double\" 'single' $HOME [^ ]*"
sed -E 's/.*="?([0-9.]+):.*/\1/' /etc/hostname
`

func TestScriptInterpreterFor(t *testing.T) {
	tests := []struct {
		os, name string
		want     ScriptInterpreter
	}{
		{"linux", "foo.sh", ScriptSh},
		{"linux", "foo.ps1", ScriptSh}, // the node's OS wins on linux
		{"darwin", "", ScriptSh},
		{"windows", "fix.ps1", ScriptPowerShell},
		{"windows", `C:\scripts\FIX.PS1`, ScriptPowerShell},
		{"windows", "build.cmd", ScriptCmd},
		{"windows", `.\build.BAT`, ScriptCmd},
		{"windows", "", ScriptPowerShell}, // stdin has no name
		{"windows", "notes.txt", ScriptPowerShell},
	}
	for _, tc := range tests {
		if got := ScriptInterpreterFor(tc.os, tc.name); got != tc.want {
			t.Errorf("ScriptInterpreterFor(%q, %q) = %q, want %q", tc.os, tc.name, got, tc.want)
		}
	}
}

func TestPrepareScriptBody(t *testing.T) {
	bom := string(utf8BOM)
	tests := []struct {
		name   string
		interp ScriptInterpreter
		in     string
		want   string
	}{
		{"sh keeps LF", ScriptSh, "a\nb\n", "a\nb\n"},
		{"sh drops CR from CRLF", ScriptSh, "if true; then\r\n  echo x\r\nfi\r\n", "if true; then\n  echo x\nfi\n"},
		{"sh leaves quotes alone", ScriptSh, quoteSoup, quoteSoup},
		// What PowerShell 5.1 actually pipes: BOM + CRLF (seen live on
		// mfl-linux-arm64-106 as "\xef\xbb\xbfuname: not found").
		{"sh drops a piped BOM", ScriptSh, bom + "uname -a\r\n", "uname -a\n"},
		{"cmd drops a piped BOM", ScriptCmd, bom + "ver\n", "ver\r\n"},
		{"powershell gains a BOM", ScriptPowerShell, "Write-Output 'é'\n", bom + "Write-Output 'é'\n"},
		{"powershell keeps an existing BOM", ScriptPowerShell, bom + "x", bom + "x"},
		{"powershell keeps a UTF-16 BOM", ScriptPowerShell, "\xff\xfex\x00", "\xff\xfex\x00"},
		{"cmd gets CRLF", ScriptCmd, "@echo off\ngoto :x\r\n:x\n", "@echo off\r\ngoto :x\r\n:x\r\n"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := string(prepareScriptBody(tc.interp, []byte(tc.in))); got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// Over the guest agent the script is file contents, written verbatim and run
// by path; its args are separate argv elements. Nothing is joined into a
// command line.
func TestHypervisorExecScriptLinux(t *testing.T) {
	pinToken(t, "tok")
	inst := newScriptInstance()
	inst.run = provider.ExecStatus{ExitCode: 3, OutData: "out", ErrData: "err"}

	res, err := scriptExecutor("linux", inst).ExecScript(context.Background(),
		NodeScript{Body: []byte(quoteSoup), Name: "probe.sh", Args: []string{"a b", `"q"`}}, 30)
	if err != nil {
		t.Fatal(err)
	}
	const path = "/tmp/mayfly-exec-tok.sh"
	if got := string(inst.writes[path]); got != quoteSoup {
		t.Errorf("guest file = %q, want the script verbatim %q", got, quoteSoup)
	}
	if want := []string{"/bin/sh", path, "a b", `"q"`}; !reflect.DeepEqual(inst.runArgv, want) {
		t.Errorf("run argv = %q, want %q", inst.runArgv, want)
	}
	if res.ExitCode != 3 || res.Stdout != "out" || res.Stderr != "err" {
		t.Errorf("result = %+v, want the script's own exit/stdout/stderr", res)
	}
	if last := inst.execs[len(inst.execs)-1]; !reflect.DeepEqual(last, []string{"rm", "-f", "--", path}) {
		t.Errorf("last exec = %q, want the script removed", last)
	}
}

// A #! line means the script wants its own interpreter (bash arrays, python):
// it is made executable and exec'd rather than fed to /bin/sh.
func TestHypervisorExecScriptShebang(t *testing.T) {
	pinToken(t, "tok")
	inst := newScriptInstance()
	body := "#!/bin/bash\narr=(1 2)\necho ${arr[1]}\n"

	if _, err := scriptExecutor("linux", inst).ExecScript(context.Background(),
		NodeScript{Body: []byte(body), Args: []string{"x"}}, 30); err != nil {
		t.Fatal(err)
	}
	want := []string{"/bin/sh", "-c", shebangExec, "/tmp/mayfly-exec-tok.sh", "x"}
	if !reflect.DeepEqual(inst.runArgv, want) {
		t.Errorf("run argv = %q, want %q", inst.runArgv, want)
	}
}

func TestHypervisorExecScriptWindows(t *testing.T) {
	pinToken(t, "tok")
	tests := []struct {
		name     string
		file     string
		body     string
		path     string
		wantBody string
		argv     []string
	}{
		{
			name: "ps1 runs under powershell -File", file: "fix.ps1",
			body: "Write-Output \"a `\"b`\"\"\n", path: `C:\Windows\Temp\mayfly-exec-tok.ps1`,
			wantBody: string(utf8BOM) + "Write-Output \"a `\"b`\"\"\n",
			argv: []string{"powershell.exe", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass",
				"-File", `C:\Windows\Temp\mayfly-exec-tok.ps1`, "arg"},
		},
		{
			name: "stdin defaults to powershell", file: "",
			body: "hostname", path: `C:\Windows\Temp\mayfly-exec-tok.ps1`,
			wantBody: string(utf8BOM) + "hostname",
			argv: []string{"powershell.exe", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass",
				"-File", `C:\Windows\Temp\mayfly-exec-tok.ps1`, "arg"},
		},
		{
			name: "cmd runs as a batch file", file: "build.cmd",
			body: "@echo off\necho %1\n", path: `C:\Windows\Temp\mayfly-exec-tok.cmd`,
			wantBody: "@echo off\r\necho %1\r\n",
			argv:     []string{"cmd.exe", "/d", "/c", `C:\Windows\Temp\mayfly-exec-tok.cmd`, "arg"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			inst := newScriptInstance()
			if _, err := scriptExecutor("windows", inst).ExecScript(context.Background(),
				NodeScript{Body: []byte(tc.body), Name: tc.file, Args: []string{"arg"}}, 30); err != nil {
				t.Fatal(err)
			}
			if got := string(inst.writes[tc.path]); got != tc.wantBody {
				t.Errorf("guest file %s = %q, want %q", tc.path, got, tc.wantBody)
			}
			if !reflect.DeepEqual(inst.runArgv, tc.argv) {
				t.Errorf("run argv = %q, want %q", inst.runArgv, tc.argv)
			}
			wantRm := []string{"cmd.exe", "/d", "/c", "del", "/f", "/q", tc.path}
			if last := inst.execs[len(inst.execs)-1]; !reflect.DeepEqual(last, wantRm) {
				t.Errorf("last exec = %q, want %q", last, wantRm)
			}
		})
	}
}

// A script over one file-write's payload cap is written as ordered chunks and
// joined in the guest; joined, the chunks must be the script exactly, and the
// chunks are removed along with it.
func TestHypervisorExecScriptChunksLargeScripts(t *testing.T) {
	pinToken(t, "tok")
	prev := scriptChunkBytes
	scriptChunkBytes = 10
	t.Cleanup(func() { scriptChunkBytes = prev })

	for _, osName := range []string{"linux", "windows"} {
		t.Run(osName, func(t *testing.T) {
			inst := newScriptInstance()
			body := []byte(strings.Repeat("echo 'x'\n", 4)) // 36 bytes -> 4 chunks
			if osName == "windows" {
				body = append(append([]byte(nil), utf8BOM...), body...)
			}
			ex := scriptExecutor(osName, inst)
			if _, err := ex.ExecScript(context.Background(), NodeScript{Body: body}, 30); err != nil {
				t.Fatal(err)
			}
			if len(inst.order) != 4 {
				t.Fatalf("wrote %d chunks (%q), want 4", len(inst.order), inst.order)
			}
			var joined []byte
			for _, p := range inst.order {
				if len(inst.writes[p]) > scriptChunkBytes {
					t.Errorf("chunk %s is %d bytes, over the %d cap", p, len(inst.writes[p]), scriptChunkBytes)
				}
				joined = append(joined, inst.writes[p]...)
			}
			if !bytes.Equal(joined, body) {
				t.Errorf("chunks join to %q, want %q", joined, body)
			}
			interp := ScriptInterpreterFor(osName, "")
			path := guestScriptPath(interp, "tok")
			if want := scriptJoinArgv(interp, path, inst.order); !reflect.DeepEqual(inst.execs[0], want) {
				t.Errorf("first exec = %q, want the join %q", inst.execs[0], want)
			}
			wantRm := scriptRemoveArgv(interp, append(append([]string(nil), inst.order...), path))
			if last := inst.execs[len(inst.execs)-1]; !reflect.DeepEqual(last, wantRm) {
				t.Errorf("last exec = %q, want %q", last, wantRm)
			}
		})
	}
}

// When the run's outcome is unknown the script may still be executing, so it
// must stay put — and the error has to say where it is.
func TestHypervisorExecScriptLostLeavesScript(t *testing.T) {
	pinToken(t, "tok")
	inst := newScriptInstance()
	inst.runErr = provider.ErrExecStatusLost

	_, err := scriptExecutor("linux", inst).ExecScript(context.Background(), NodeScript{Body: []byte("sleep 999")}, 1)
	if !errors.Is(err, provider.ErrExecStatusLost) {
		t.Fatalf("error = %v, want ErrExecStatusLost", err)
	}
	if !strings.Contains(err.Error(), "/tmp/mayfly-exec-tok.sh") {
		t.Errorf("error %q does not name the script left behind", err)
	}
	for _, argv := range inst.execs {
		if argv[0] == "rm" {
			t.Errorf("removed the script of a command that may still be running: %q", argv)
		}
	}
}

// captureSSH swaps the SSH transport for one that records the command line and
// stdin it would have sent.
func captureSSH(t *testing.T, out string, err error) (*string, *[]byte) {
	t.Helper()
	var cmd string
	var in []byte
	prev := sshRun
	sshRun = func(_ context.Context, _ SSHTarget, script string, stdin []byte) (string, error) {
		cmd, in = script, append([]byte(nil), stdin...)
		return out, err
	}
	t.Cleanup(func() { sshRun = prev })
	return &cmd, &in
}

// Over SSH the script rides stdin, and the command line is only the fixed
// wrapper plus mayfly-quoted args — the script's bytes never appear on it.
func TestSSHExecScriptSendsScriptOnStdin(t *testing.T) {
	cmd, in := captureSSH(t, "hi\n", nil)
	ex := &sshExecutor{target: SSHTarget{User: "u", Host: "h"}, os: "darwin"}

	res, err := ex.ExecScript(context.Background(),
		NodeScript{Body: []byte(strings.ReplaceAll(quoteSoup, "\n", "\r\n")), Args: []string{"it's", "two words"}}, 30)
	if err != nil {
		t.Fatal(err)
	}
	if string(*in) != quoteSoup {
		t.Errorf("stdin = %q, want the script (CRLF folded) %q", *in, quoteSoup)
	}
	if strings.Contains(*cmd, "uname") {
		t.Errorf("command line carries the script body: %q", *cmd)
	}
	if want := `set -- 'it'\''s' 'two words'` + "\n"; !strings.HasPrefix(*cmd, want) {
		t.Errorf("command line = %q, want it to start with %q", *cmd, want)
	}
	if !strings.Contains(*cmd, `sh "$f" "$@" < /dev/null`) {
		t.Errorf("command line = %q, want it to run the temp file with sh", *cmd)
	}
	if res.Stdout != "hi\n" {
		t.Errorf("stdout = %q", res.Stdout)
	}
}

// The wrapper is real sh: run it locally, the way the node's `sudo sh -c`
// would, and check the script sees its args and quotes intact and its exit
// status survives the cleanup trap.
func TestSSHScriptWrapperRunsUnderSh(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh on PATH")
	}
	body := `printf '%s|' "$#" "$1" "$2"; echo "\"double\" 'single' [^ ]*"; exit 7` + "\n"
	for _, shebang := range []bool{false, true} {
		b := body
		if shebang {
			b = "#!/bin/sh\n" + body
		}
		c := exec.Command(sh, "-c", sshScriptCommand(NodeScript{Args: []string{"a b", "it's"}}, shebang))
		c.Stdin = strings.NewReader(b)
		out, err := c.Output()
		var ee *exec.ExitError
		if !errors.As(err, &ee) || ee.ExitCode() != 7 {
			t.Fatalf("shebang=%v: err = %v (out %q), want exit 7", shebang, err, out)
		}
		if want := "2|a b|it's|\"double\" 'single' [^ ]*\n"; string(out) != want {
			t.Errorf("shebang=%v: out = %q, want %q", shebang, out, want)
		}
	}
}

func TestSSHExecScriptRecoversExitCode(t *testing.T) {
	captureSSH(t, "partial", wrapExit(t, 4))
	ex := &sshExecutor{target: SSHTarget{User: "u", Host: "h"}, os: "linux"}
	res, err := ex.ExecScript(context.Background(), NodeScript{Body: []byte("exit 4")}, 30)
	if err != nil || res.ExitCode != 4 || res.Stdout != "partial" {
		t.Errorf("got (%+v, %v), want exit 4 with its output", res, err)
	}
}

func TestSSHExecScriptRefusesWindows(t *testing.T) {
	captureSSH(t, "", nil)
	ex := &sshExecutor{target: SSHTarget{User: "u", Host: "h"}, os: "windows"}
	if _, err := ex.ExecScript(context.Background(), NodeScript{Body: []byte("x")}, 30); !errors.Is(err, errScriptWindowsSSH) {
		t.Errorf("error = %v, want errScriptWindowsSSH", err)
	}
}

// The registry cache and monitoring host are reachable by hostname or role,
// over the guest agent on the node state recorded for them.
func TestResolveServiceVMTarget(t *testing.T) {
	st := &state.Store{
		RegistryCache: &state.RegistryCache{VMID: 101, Hostname: "mfl-registry-cache-101", PVENode: "node-b"},
		Monitoring:    &state.Monitoring{VMID: 103, Hostname: "mfl-monitoring-103", PVENode: "node-a"},
	}
	for _, tc := range []struct {
		name string
		id   int
		node string
	}{
		{"mfl-registry-cache-101", 101, "node-b"},
		{"registry-cache", 101, "node-b"},
		{"mfl-monitoring-103", 103, "node-a"},
		{"monitoring", 103, "node-a"},
	} {
		got, ok := ResolveServiceVMTarget(st, tc.name)
		if !ok || got.Transport != NodeTransportHypervisor || got.ID != tc.id || got.Node != tc.node || got.OS != "linux" {
			t.Errorf("%s: got (%+v, %v), want hypervisor vm %d on %s", tc.name, got, ok, tc.id, tc.node)
		}
	}
	if _, ok := ResolveServiceVMTarget(st, "mfl-linux-amd64-100"); ok {
		t.Error("a runner name resolved as a service VM")
	}
	if _, ok := ResolveServiceVMTarget(&state.Store{}, "registry-cache"); ok {
		t.Error("registry-cache resolved with no cache VM in state")
	}
}
