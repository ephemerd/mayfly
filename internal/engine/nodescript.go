package engine

// `mayfly node exec --script` — run a local script file on a node.
//
// Passing a script as an argv means it crosses every shell between the
// operator and the guest: local PowerShell 5.1 (which mangles embedded double
// quotes on the way to a native exe) or Git Bash (which rewrites anything that
// looks like a Unix path), then whatever the guest-side command line parses.
// A one-line sed with a quoted regex was enough to break that chain.
//
// A script is therefore treated as DATA end to end. mayfly reads the bytes
// itself and hands them to the guest as file contents, never as part of a
// command line:
//
//   - Over the hypervisor channel the bytes go through the guest agent's
//     file-write (base64 on the wire, binary-safe) to a fresh temp file, which
//     is then executed by path and removed. file-write has a per-call payload
//     cap, so a large script is written in chunks and joined in the guest.
//   - Over SSH the bytes ride the session's stdin into a mktemp file, which is
//     then executed by path and removed. The command line only ever carries a
//     fixed wrapper plus the operator's positional args, quoted by mayfly.
//
// Executing a FILE rather than piping the script into `sh -s` keeps the
// script's own stdin free: a command inside it that reads stdin (apt, ssh, a
// `read`) cannot swallow the rest of the script.

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"path"
	"strings"
)

// NodeScript is a script to run on a node.
type NodeScript struct {
	// Body is the script's bytes, exactly as read from the file or stdin.
	Body []byte
	// Name is the local file name the script came from ("" for stdin). Only
	// its extension matters: it picks the interpreter on Windows.
	Name string
	// Args are passed to the script as its positional parameters.
	Args []string
}

// ScriptInterpreter is what runs a script on the node.
type ScriptInterpreter string

const (
	// ScriptSh runs the script with /bin/sh, or with its own interpreter when
	// it starts with a #! line. Linux and darwin.
	ScriptSh ScriptInterpreter = "sh"
	// ScriptPowerShell runs the script with `powershell.exe -File`. Windows.
	ScriptPowerShell ScriptInterpreter = "powershell"
	// ScriptCmd runs the script as a batch file under cmd.exe. Windows.
	ScriptCmd ScriptInterpreter = "cmd"
)

// ScriptInterpreterFor picks the interpreter for a script on a node of osName.
//
// Linux and darwin always use sh (a #! line still wins, see scriptRunArgv).
// Windows has two native shells and nothing in the bytes says which one a
// script was written for, so the file extension decides: .cmd and .bat are
// batch files for cmd.exe, and everything else — .ps1, any other extension,
// and stdin, which has no name at all — is PowerShell, the shell the rest of
// mayfly's Windows tooling speaks.
func ScriptInterpreterFor(osName, name string) ScriptInterpreter {
	if osName != "windows" {
		return ScriptSh
	}
	switch strings.ToLower(path.Ext(strings.ReplaceAll(name, `\`, "/"))) {
	case ".cmd", ".bat":
		return ScriptCmd
	default:
		return ScriptPowerShell
	}
}

// utf8BOM marks a file as UTF-8 for Windows PowerShell 5.1, which otherwise
// reads a .ps1 in the system ANSI code page and mangles any non-ASCII byte.
var utf8BOM = []byte{0xEF, 0xBB, 0xBF}

// prepareScriptBody adapts line endings and encoding to the interpreter. It is
// the only place the bytes are touched, and only for things the interpreter
// would otherwise misread — never quoting or content:
//
//   - sh: CRLF becomes LF. The operator authors on Windows, and a CR left at a
//     line end turns `fi` into `fi\r` ("syntax error: unexpected end of file").
//   - sh and cmd: a leading UTF-8 BOM is dropped. Windows PowerShell 5.1
//     prepends one to anything it pipes into a native exe (`Get-Content x |
//     mayfly ... --script -`), and both shells read it as part of the first
//     command ("\xef\xbb\xbfuname: not found").
//   - powershell: a UTF-8 BOM is added when the file has no BOM, so PowerShell
//     5.1 decodes it as UTF-8 rather than ANSI. Harmless for pure ASCII.
//   - cmd: bare LF becomes CRLF. cmd.exe mis-seeks labels in LF-only batch
//     files, so `goto`/`call :label` can land in the wrong place.
func prepareScriptBody(interp ScriptInterpreter, body []byte) []byte {
	switch interp {
	case ScriptSh:
		body = bytes.TrimPrefix(body, utf8BOM)
		return bytes.ReplaceAll(body, []byte("\r\n"), []byte("\n"))
	case ScriptPowerShell:
		if hasBOM(body) {
			return body
		}
		return append(append([]byte(nil), utf8BOM...), body...)
	case ScriptCmd:
		lf := bytes.ReplaceAll(bytes.TrimPrefix(body, utf8BOM), []byte("\r\n"), []byte("\n"))
		return bytes.ReplaceAll(lf, []byte("\n"), []byte("\r\n"))
	}
	return body
}

// hasBOM reports whether body already declares its encoding (UTF-8 or UTF-16).
func hasBOM(body []byte) bool {
	return bytes.HasPrefix(body, utf8BOM) ||
		bytes.HasPrefix(body, []byte{0xFF, 0xFE}) ||
		bytes.HasPrefix(body, []byte{0xFE, 0xFF})
}

// hasShebang reports whether a sh-family script names its own interpreter.
func hasShebang(body []byte) bool { return bytes.HasPrefix(body, []byte("#!")) }

// scriptToken makes the temp file names unique. A var so tests can pin it.
var scriptToken = func() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// guestScriptPath is where the script lands in the guest. The extension is
// load-bearing on Windows: powershell -File insists on .ps1, and cmd.exe only
// runs a file as a batch script when it ends in .cmd or .bat.
func guestScriptPath(interp ScriptInterpreter, token string) string {
	switch interp {
	case ScriptPowerShell:
		return `C:\Windows\Temp\mayfly-exec-` + token + ".ps1"
	case ScriptCmd:
		return `C:\Windows\Temp\mayfly-exec-` + token + ".cmd"
	default:
		return "/tmp/mayfly-exec-" + token + ".sh"
	}
}

// shebangExec is the sh wrapper for a script with a #! line: make it
// owner-only executable and exec it, so its own interpreter (bash, python)
// runs it. $0 is the script path, so it sees the same $0 it would directly.
const shebangExec = `chmod 700 "$0" && exec "$0" "$@"`

// scriptRunArgv is the argv that runs the script at guestPath. Each arg is a
// separate argv element; the guest agent spawns it without a shell.
func scriptRunArgv(interp ScriptInterpreter, guestPath string, shebang bool, args []string) []string {
	var argv []string
	switch interp {
	case ScriptPowerShell:
		argv = []string{"powershell.exe", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-File", guestPath}
	case ScriptCmd:
		// /d skips AutoRun, so a node's registry cannot inject commands
		// ahead of the script.
		argv = []string{"cmd.exe", "/d", "/c", guestPath}
	default:
		if shebang {
			argv = []string{"/bin/sh", "-c", shebangExec, guestPath}
		} else {
			argv = []string{"/bin/sh", guestPath}
		}
	}
	return append(argv, args...)
}

// scriptRemoveArgv deletes the script (and any chunk files) from the guest.
func scriptRemoveArgv(interp ScriptInterpreter, paths []string) []string {
	if interp == ScriptSh {
		return append([]string{"rm", "-f", "--"}, paths...)
	}
	return append([]string{"cmd.exe", "/d", "/c", "del", "/f", "/q"}, paths...)
}

// scriptJoinArgv concatenates chunk files into out, in order.
func scriptJoinArgv(interp ScriptInterpreter, out string, parts []string) []string {
	if interp == ScriptSh {
		return append([]string{"/bin/sh", "-c", `out=$1; shift; cat -- "$@" > "$out"`, "sh", out}, parts...)
	}
	// copy /b a+b+c out: binary concatenation, no EOF marker appended.
	return []string{"cmd.exe", "/d", "/c", "copy", "/b", "/y", strings.Join(parts, "+"), out}
}

// scriptChunkBytes is the largest piece written in one guest-agent file-write.
// PVE caps the (base64) content of one call at 60 KiB, which is 45 KiB raw;
// 32 KiB leaves headroom for the rest of the request.
var scriptChunkBytes = 32 * 1024

// scriptHelperTimeoutSec bounds the bookkeeping execs (join, remove), which do
// no real work.
const scriptHelperTimeoutSec = 60

// ExecScript writes the script into the guest through the agent's file-write
// and runs it by path. See the file comment for why it is a file.
//
// The script is removed afterwards only when its outcome is known. If the
// exec is lost (guest agent gap, timeout), the script may still be running —
// and cmd.exe reads a batch file as it goes — so it is left in place and the
// error names it.
func (h *hypervisorExecutor) ExecScript(ctx context.Context, s NodeScript, timeoutSec int) (NodeExecResult, error) {
	if err := h.instance(ctx); err != nil {
		return NodeExecResult{}, err
	}
	interp := ScriptInterpreterFor(h.os, s.Name)
	body := prepareScriptBody(interp, s.Body)
	guestPath := guestScriptPath(interp, scriptToken())

	parts, err := h.writeScript(ctx, interp, guestPath, body)
	if err != nil {
		return NodeExecResult{}, err
	}

	st, err := h.inst.Exec(ctx, scriptRunArgv(interp, guestPath, hasShebang(body), s.Args), timeoutSec)
	if err != nil {
		return NodeExecResult{}, fmt.Errorf("%w (script left at %s on %s)", err, guestPath, h.name)
	}
	res := NodeExecResult{Stdout: st.OutData, Stderr: st.ErrData, ExitCode: st.ExitCode}
	if rerr := h.removeScript(ctx, interp, append(parts, guestPath)); rerr != nil {
		res.Stderr += fmt.Sprintf("mayfly: could not remove %s from %s: %v\n", guestPath, h.name, rerr)
	}
	return res, nil
}

// writeScript lands body at guestPath. A body that fits one file-write is
// written directly; a larger one is written as numbered chunk files and joined
// in the guest. Returns the chunk paths, which the caller removes with the
// script.
func (h *hypervisorExecutor) writeScript(ctx context.Context, interp ScriptInterpreter, guestPath string, body []byte) ([]string, error) {
	if len(body) <= scriptChunkBytes {
		if err := h.inst.FileWrite(ctx, guestPath, body); err != nil {
			return nil, fmt.Errorf("write script to %s on %s: %w", guestPath, h.name, err)
		}
		return nil, nil
	}

	var parts []string
	for i := 0; len(body) > 0; i++ {
		n := min(len(body), scriptChunkBytes)
		part := fmt.Sprintf("%s.part%03d", guestPath, i)
		if err := h.inst.FileWrite(ctx, part, body[:n]); err != nil {
			_ = h.removeScript(ctx, interp, parts)
			return nil, fmt.Errorf("write script chunk %s on %s: %w", part, h.name, err)
		}
		parts = append(parts, part)
		body = body[n:]
	}
	st, err := h.inst.Exec(ctx, scriptJoinArgv(interp, guestPath, parts), scriptHelperTimeoutSec)
	if err == nil && st.ExitCode != 0 {
		err = fmt.Errorf("exit code %d: %s", st.ExitCode, strings.TrimSpace(st.OutData+st.ErrData))
	}
	if err != nil {
		_ = h.removeScript(ctx, interp, append(parts, guestPath))
		return nil, fmt.Errorf("join script chunks on %s: %w", h.name, err)
	}
	return parts, nil
}

func (h *hypervisorExecutor) removeScript(ctx context.Context, interp ScriptInterpreter, paths []string) error {
	if len(paths) == 0 {
		return nil
	}
	st, err := h.inst.Exec(ctx, scriptRemoveArgv(interp, paths), scriptHelperTimeoutSec)
	if err == nil && st.ExitCode != 0 {
		err = fmt.Errorf("exit code %d", st.ExitCode)
	}
	return err
}

// sshScriptWrapper is the fixed command line for a script over SSH. It runs
// under the transport's `sudo sh -c`, with the script bytes on stdin:
//
//   - mktemp + cat land stdin in an owner-only temp file. Exit 125 marks a
//     failure of the wrapper itself rather than of the script.
//   - the EXIT trap removes it however the script ends; a trap that does not
//     call exit leaves the script's status as the shell's.
//   - the script's stdin is /dev/null: the session's stdin was the script and
//     is spent.
//
// %s is the run line, chosen by sshScriptCommand.
const sshScriptWrapper = `f=$(mktemp /tmp/mayfly-exec.XXXXXX) || exit 125
trap 'rm -f "$f"' EXIT
cat > "$f" || exit 125
%s "$@" < /dev/null
`

// sshScriptCommand builds the command line for running s over SSH. The
// operator's args become the wrapper's positional parameters via `set --`,
// quoted by ShellJoin — the only quoting on the path, and mayfly's own.
func sshScriptCommand(s NodeScript, shebang bool) string {
	run := `sh "$f"`
	if shebang {
		run = `chmod 700 "$f" && "$f"`
	}
	return "set -- " + ShellJoin(s.Args) + "\n" + fmt.Sprintf(sshScriptWrapper, run)
}

// errScriptWindowsSSH: no Windows node is reached over SSH today, and the
// wrapper above is POSIX sh.
var errScriptWindowsSSH = errors.New("--script over ssh supports linux and darwin nodes only")

// ExecScript feeds the script on the session's stdin into a temp file on the
// node and runs it. See sshScriptWrapper.
func (s *sshExecutor) ExecScript(ctx context.Context, sc NodeScript, timeoutSec int) (NodeExecResult, error) {
	if s.os == "windows" {
		return NodeExecResult{}, errScriptWindowsSSH
	}
	body := prepareScriptBody(ScriptSh, sc.Body)
	return s.run(ctx, sshScriptCommand(sc, hasShebang(body)), body, timeoutSec)
}
