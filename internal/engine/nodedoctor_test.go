package engine

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// fakeNodeExecutor answers a check's argv with a canned result, so the doctor
// battery can be exercised end-to-end (build argv -> run -> evaluate) without a
// real node. Keyed by the whole joined argv because two checks share argv[0]
// (`ephemerd status` and `ephemerd --version`).
type fakeNodeExecutor struct {
	out map[string]NodeExecResult
	err map[string]error
}

func (f fakeNodeExecutor) Exec(_ context.Context, argv []string, _ int) (NodeExecResult, error) {
	key := strings.Join(argv, " ")
	if e, ok := f.err[key]; ok {
		return NodeExecResult{}, e
	}
	return f.out[key], nil
}

func (f fakeNodeExecutor) ExecScript(context.Context, NodeScript, int) (NodeExecResult, error) {
	return NodeExecResult{}, errors.New("fakeNodeExecutor: scripts are not part of the doctor battery")
}

// findCheck returns the named check from a battery.
func findCheck(t *testing.T, checks []DoctorCheck, name string) DoctorCheck {
	t.Helper()
	for _, c := range checks {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("no check named %q in battery", name)
	return DoctorCheck{}
}

// TestNodeDoctorEval drives every linux check through the fake executor and
// asserts the verdict for both a healthy and an unhealthy probe output. The
// fake NodeExecutor is the seam: each row is a raw probe result mapped to an
// expected pass/fail.
func TestNodeDoctorEval(t *testing.T) {
	checks := NodeDoctorChecks("linux")

	tests := []struct {
		name     string
		check    string
		res      NodeExecResult
		wantPass bool
		detail   string // substring expected in the rendered detail
	}{
		{"ephemerd up", "ephemerd active", NodeExecResult{ExitCode: 0}, true, "responding"},
		{"ephemerd down", "ephemerd active", NodeExecResult{ExitCode: 3}, false, "exited 3"},

		{"version parses", "ephemerd version", NodeExecResult{Stdout: "ephemerd v0.2.6 (abc)"}, true, "v0.2.6"},
		{"version on stderr", "ephemerd version", NodeExecResult{Stderr: "ephemerd version 0.2.6"}, true, "v0.2.6"},
		{"no version", "ephemerd version", NodeExecResult{Stdout: "wedged"}, false, "no version"},

		{"bridge on", "bridge netfilter", NodeExecResult{Stdout: "1\n"}, true, "= 1"},
		{"bridge off", "bridge netfilter", NodeExecResult{Stdout: "0\n"}, false, "= 0"},
		{"bridge unset", "bridge netfilter", NodeExecResult{Stdout: ""}, false, "unset"},

		{"c2c has drop", "c2c drop chain",
			NodeExecResult{ExitCode: 0, Stdout: "-N EPHEMERD-C2C\n-A EPHEMERD-C2C -j DROP\n"}, true, "DROP"},
		{"c2c present no drop", "c2c drop chain",
			NodeExecResult{ExitCode: 0, Stdout: "-N EPHEMERD-C2C\n"}, false, "no DROP"},
		{"c2c missing", "c2c drop chain",
			NodeExecResult{ExitCode: 1, Stderr: "iptables: No chain/target/match by that name."}, false, "not present"},

		{"dns resolves", "host dns resolves",
			NodeExecResult{ExitCode: 0, Stdout: "140.82.112.3    github.com\n"}, true, "host netns"},
		{"dns fails", "host dns resolves", NodeExecResult{ExitCode: 2, Stdout: ""}, false, "no answer"},

		{"disk ok", "disk headroom",
			NodeExecResult{Stdout: "Filesystem 1024-blocks Used Available Capacity Mounted on\n/dev/sda1 100 12 88 12% /\n"}, true, "12% used"},
		{"disk full", "disk headroom",
			NodeExecResult{Stdout: "Filesystem 1024-blocks Used Available Capacity Mounted on\n/dev/sda1 100 95 5 95% /\n"}, false, "95% used"},
		{"disk unreadable", "disk headroom", NodeExecResult{Stdout: "df: /var/lib/ephemerd: No such file"}, false, "could not read"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ck := findCheck(t, checks, tc.check)
			fake := fakeNodeExecutor{out: map[string]NodeExecResult{strings.Join(ck.Argv, " "): tc.res}}
			got, err := fake.Exec(context.Background(), ck.Argv, 0)
			if err != nil {
				t.Fatalf("fake exec: %v", err)
			}
			pass, detail := ck.Eval(got)
			if pass != tc.wantPass {
				t.Errorf("%s: pass = %v, want %v (detail %q)", tc.check, pass, tc.wantPass, detail)
			}
			if tc.detail != "" && !strings.Contains(detail, tc.detail) {
				t.Errorf("%s: detail = %q, want it to contain %q", tc.check, detail, tc.detail)
			}
		})
	}
}

// A whole healthy node passes every linux check when driven through the fake
// executor — the integration of build-argv, run, evaluate.
func TestNodeDoctorBatteryAllHealthy(t *testing.T) {
	checks := NodeDoctorChecks("linux")
	healthy := map[string]NodeExecResult{
		"ephemerd active":   {ExitCode: 0},
		"ephemerd version":  {Stdout: "ephemerd v0.2.6"},
		"bridge netfilter":  {Stdout: "1\n"},
		"c2c drop chain":    {ExitCode: 0, Stdout: "-A EPHEMERD-C2C -j DROP\n"},
		"host dns resolves": {ExitCode: 0, Stdout: "140.82.112.3 github.com\n"},
		"disk headroom":     {Stdout: "Filesystem 1K Used Avail Cap Mounted\n/dev/sda1 100 40 60 40% /\n"},
	}
	fake := fakeNodeExecutor{out: map[string]NodeExecResult{}}
	for _, ck := range checks {
		fake.out[strings.Join(ck.Argv, " ")] = healthy[ck.Name]
	}
	for _, ck := range checks {
		res, _ := fake.Exec(context.Background(), ck.Argv, 0)
		if pass, detail := ck.Eval(res); !pass {
			t.Errorf("check %q failed on healthy input: %s", ck.Name, detail)
		}
	}
}

// An exec-level error (unreachable node / dead channel) must not be swallowed:
// the check has no output to evaluate, and doctor treats that as a failure.
func TestNodeDoctorExecErrorSurfaces(t *testing.T) {
	ck := findCheck(t, NodeDoctorChecks("linux"), "ephemerd active")
	key := strings.Join(ck.Argv, " ")
	fake := fakeNodeExecutor{err: map[string]error{key: errors.New("guest agent not responding")}}
	if _, err := fake.Exec(context.Background(), ck.Argv, 0); err == nil {
		t.Fatal("expected the exec error to propagate")
	}
}

// windows and darwin get only the transport-agnostic ephemerd checks; the
// linux-only bridge/iptables/DNS/disk checks are marked n/a (Argv empty, NA
// set) rather than run.
func TestNodeDoctorChecksReducedOffLinux(t *testing.T) {
	for _, osName := range []string{"windows", "darwin"} {
		t.Run(osName, func(t *testing.T) {
			ran, skipped := 0, 0
			for _, ck := range NodeDoctorChecks(osName) {
				if ck.NA != "" {
					skipped++
					if len(ck.Argv) != 0 {
						t.Errorf("%s: n/a check %q still carries argv %q", osName, ck.Name, ck.Argv)
					}
					continue
				}
				ran++
				if len(ck.Argv) == 0 {
					t.Errorf("%s: runnable check %q has no argv", osName, ck.Name)
				}
			}
			if ran != 2 {
				t.Errorf("%s: %d runnable checks, want 2 (ephemerd active + version)", osName, ran)
			}
			if skipped != 4 {
				t.Errorf("%s: %d n/a checks, want 4", osName, skipped)
			}
		})
	}
}

// Linux runs the full battery, every check with a real argv.
func TestNodeDoctorChecksLinuxFull(t *testing.T) {
	checks := NodeDoctorChecks("linux")
	if len(checks) != 6 {
		t.Fatalf("linux battery has %d checks, want 6", len(checks))
	}
	for _, ck := range checks {
		if ck.NA != "" {
			t.Errorf("linux check %q is marked n/a, want it to run", ck.Name)
		}
		if len(ck.Argv) == 0 {
			t.Errorf("linux check %q has no argv", ck.Name)
		}
	}
}

func TestParseDfUsedPercent(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want int
		ok   bool
	}{
		{"typical", "Filesystem 1024-blocks Used Available Capacity Mounted on\n/dev/sda1 100 12 88 12% /\n", 12, true},
		{"full", "hdr\n/dev/sda1 100 100 0 100% /", 100, true},
		{"no percent field", "hdr\ngarbage without a cap", 0, false},
		{"empty", "", 0, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := parseDfUsedPercent(tc.in)
			if ok != tc.ok || got != tc.want {
				t.Errorf("parseDfUsedPercent = (%d, %v), want (%d, %v)", got, ok, tc.want, tc.ok)
			}
		})
	}
}
