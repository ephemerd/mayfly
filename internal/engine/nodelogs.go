package engine

// Log tailing for `mayfly node logs`.
//
// Where a node writes ephemerd's logs depends on how its service is managed, so
// the command mayfly runs is OS-specific, the same way the upgrade push is:
//   - linux runs ephemerd under systemd, so its logs are in journald.
//   - darwin runs ephemerd under launchd (see enrollServiceStart / `ephemerd
//     install`), which has no journald — macOS has no `journalctl` at all, so a
//     journalctl invocation there fails with exit 127. Its logs go to the
//     unified logging system, read with `log show` / `log stream`.
//   - windows runs ephemerd as a service logging to the Windows Event Log, read
//     with Get-WinEvent.
//
// LogsArgv owns that branching and nothing else: it is pure, so a table test
// pins the exact argv per OS.
//
// A note on -f over the guest agent: the hypervisor executor captures a
// command's output only after it exits, so a `-f` follow on a Proxmox VM does
// not stream live — it blocks until the exec timeout and then dumps what it
// buffered. Follow is genuinely live only on the SSH transport (the Mac). The
// caller bounds -f with a finite timeout and says so; here we just build argv.

import (
	"strconv"
	"strings"
)

// LogsArgv builds the command that tails ephemerd's logs on a node running
// osName, showing the last `lines` entries and, when follow is set, continuing
// to stream new ones.
func LogsArgv(osName string, lines int, follow bool) []string {
	if lines <= 0 {
		lines = DefaultLogLines
	}
	switch osName {
	case "windows":
		return winLogsArgv(lines, follow)
	case "darwin":
		return darwinLogsArgv(lines, follow)
	default:
		// linux (and any other systemd host): journald. --no-pager keeps
		// journalctl from trying to invoke a pager on a channel with no tty.
		args := []string{"journalctl", "-u", "ephemerd", "-n", strconv.Itoa(lines), "--no-pager"}
		if follow {
			args = append(args, "-f")
		}
		return args
	}
}

// darwinLogsArgv reads ephemerd's logs out of the macOS unified logging system.
// launchd has no journald, so `journalctl` does not exist on macOS; `log`
// always does. Follow uses `log stream` (live over SSH); a one-shot tail uses
// `log show` filtered to the ephemerd process and piped through tail for the
// line count `log show` itself cannot express, bounded to the last day so it
// does not scan the entire persisted store.
func darwinLogsArgv(lines int, follow bool) []string {
	const predicate = `process == "ephemerd"`
	if follow {
		return []string{"log", "stream", "--predicate", predicate, "--style", "syslog"}
	}
	script := "log show --predicate " + shellSingleQuote(predicate) +
		" --style syslog --info --last 1d | tail -n " + strconv.Itoa(lines)
	return []string{"/bin/sh", "-c", script}
}

// shellSingleQuote wraps s in single quotes, escaping any embedded single quote,
// so it survives as one argument inside a `/bin/sh -c` script.
func shellSingleQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// DefaultLogLines is how many trailing log entries `node logs` shows when
// --lines is not given.
const DefaultLogLines = 200

// winLogsArgv reads ephemerd's events out of the Windows Event Log. Get-WinEvent
// has no native follow, so -f is best-effort: it re-reads the tail. The caller
// warns when -f is asked for on windows; here we simply ignore it and return the
// point-in-time tail so the command still does something useful.
func winLogsArgv(lines int, _ bool) []string {
	script := "$ErrorActionPreference='SilentlyContinue'; " +
		"Get-WinEvent -FilterHashtable @{LogName='Application'; ProviderName='ephemerd'} -MaxEvents " +
		strconv.Itoa(lines) +
		" | Sort-Object TimeCreated | Format-Table -AutoSize -Wrap TimeCreated, LevelDisplayName, Message"
	return []string{"powershell", "-NoProfile", "-Command", script}
}

// LogsFollowSupported reports whether live `-f` streaming actually works on a
// node reached over the given transport. Only SSH streams; the guest agent
// buffers until the command exits, so -f there is bounded, not live.
func LogsFollowSupported(t NodeTransport) bool {
	return t == NodeTransportSSH
}
