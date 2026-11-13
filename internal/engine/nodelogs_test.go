package engine

import (
	"strings"
	"testing"
)

// LogsArgv branches on OS: journalctl on the journald platforms, Get-WinEvent on
// windows. The line count and --follow have to land in the right place for each.
func TestLogsArgv(t *testing.T) {
	tests := []struct {
		name          string
		osName        string
		lines         int
		follow        bool
		wantContains  []string
		wantHasFollow bool // journalctl -f present
		wantFirst     string
	}{
		{
			name: "linux tail", osName: "linux", lines: 50, follow: false,
			wantContains: []string{"journalctl", "-u", "ephemerd", "-n", "50", "--no-pager"},
			wantFirst:    "journalctl",
		},
		{
			name: "linux follow", osName: "linux", lines: 10, follow: true,
			wantContains: []string{"journalctl", "-n", "10", "-f"}, wantHasFollow: true,
			wantFirst: "journalctl",
		},
		{
			// darwin has no journalctl (launchd, not journald); it must read the
			// unified log and tail to the requested line count.
			name: "darwin tail uses log show", osName: "darwin", lines: 200, follow: false,
			wantContains: []string{"log show", `process == "ephemerd"`, "tail -n 200"},
			wantFirst:    "/bin/sh",
		},
		{
			name: "darwin follow uses log stream", osName: "darwin", lines: 10, follow: true,
			wantContains: []string{"stream", `process == "ephemerd"`},
			wantFirst:    "log",
		},
		{
			name: "windows uses Get-WinEvent", osName: "windows", lines: 25, follow: false,
			wantContains: []string{"Get-WinEvent", "ephemerd", "-MaxEvents 25"},
			wantFirst:    "powershell",
		},
		{
			// A non-positive line count falls back to the default rather than
			// asking journalctl for -n 0.
			name: "zero lines falls back to default", osName: "linux", lines: 0, follow: false,
			wantContains: []string{"-n", "200"}, wantFirst: "journalctl",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := LogsArgv(tc.osName, tc.lines, tc.follow)
			if got[0] != tc.wantFirst {
				t.Errorf("argv[0] = %q, want %q", got[0], tc.wantFirst)
			}
			joined := strings.Join(got, " ")
			for _, want := range tc.wantContains {
				if !strings.Contains(joined, want) {
					t.Errorf("argv %q does not contain %q", joined, want)
				}
			}
			hasFollow := false
			for _, a := range got {
				if a == "-f" {
					hasFollow = true
				}
			}
			if hasFollow != tc.wantHasFollow {
				t.Errorf("journalctl -f present = %v, want %v (argv %q)", hasFollow, tc.wantHasFollow, joined)
			}
		})
	}
}

// windows has no native journalctl -f, so even when follow is asked for the argv
// must never carry a stray -f that Get-WinEvent would reject.
func TestLogsArgvWindowsIgnoresFollow(t *testing.T) {
	got := LogsArgv("windows", 100, true)
	for _, a := range got {
		if a == "-f" {
			t.Fatalf("windows argv carried -f: %q", got)
		}
	}
}

func TestLogsFollowSupported(t *testing.T) {
	if !LogsFollowSupported(NodeTransportSSH) {
		t.Error("SSH transport should stream a live follow")
	}
	if LogsFollowSupported(NodeTransportHypervisor) {
		t.Error("guest agent buffers until exit; follow is not live there")
	}
}
