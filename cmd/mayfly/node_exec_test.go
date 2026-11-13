package main

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/urfave/cli/v3"

	"github.com/ephemerd/mayfly/internal/engine"
)

// recordingExecutor captures what `node exec` hands the transport.
type recordingExecutor struct {
	argv   []string
	script *engine.NodeScript
	tmo    int
}

func (r *recordingExecutor) Exec(_ context.Context, argv []string, tmo int) (engine.NodeExecResult, error) {
	r.argv, r.tmo = argv, tmo
	return engine.NodeExecResult{}, nil
}

func (r *recordingExecutor) ExecScript(_ context.Context, s engine.NodeScript, tmo int) (engine.NodeExecResult, error) {
	r.script, r.tmo = &s, tmo
	return engine.NodeExecResult{}, nil
}

// runExec drives `mayfly node exec <args>` against a recording executor and
// returns it along with the node name it was asked to resolve.
func runExec(t *testing.T, args ...string) (*recordingExecutor, string, error) {
	t.Helper()
	rec := &recordingExecutor{}
	var gotName string
	prev := execTarget
	execTarget = func(name string) (engine.NodeExecutor, error) {
		gotName = name
		return rec, nil
	}
	t.Cleanup(func() { execTarget = prev })

	root := &cli.Command{Name: "mayfly", Commands: []*cli.Command{
		{Name: "node", Commands: []*cli.Command{nodeExecCmd()}},
	}}
	err := root.Run(context.Background(), append([]string{"mayfly", "node", "exec"}, args...))
	return rec, gotName, err
}

func writeScript(t *testing.T, name, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// The script reaches the executor as the file's exact bytes, with the args
// after -- as its positional parameters (flag-looking ones included).
func TestNodeExecScriptFromFile(t *testing.T) {
	body := `echo "quote test: \"double\" 'single' $HOME [^ ]*"` + "\n"
	p := writeScript(t, "probe.sh", body)

	rec, name, err := runExec(t, "--script", p, "--timeout", "42", "mfl-linux-arm64-106", "--", "a b", "--flag")
	if err != nil {
		t.Fatal(err)
	}
	if name != "mfl-linux-arm64-106" {
		t.Errorf("resolved %q, want the node name", name)
	}
	if rec.script == nil {
		t.Fatal("ran a command, want a script")
	}
	if string(rec.script.Body) != body || rec.script.Name != p {
		t.Errorf("script = %q from %q, want %q from %q", rec.script.Body, rec.script.Name, body, p)
	}
	if want := []string{"a b", "--flag"}; !reflect.DeepEqual(rec.script.Args, want) {
		t.Errorf("args = %q, want %q", rec.script.Args, want)
	}
	if rec.tmo != 42 {
		t.Errorf("timeout = %d, want 42", rec.tmo)
	}
}

// The original argv form is unchanged.
func TestNodeExecCommandStillWorks(t *testing.T) {
	rec, _, err := runExec(t, "n1", "--", "ls", "-la")
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"ls", "-la"}; !reflect.DeepEqual(rec.argv, want) || rec.script != nil {
		t.Errorf("argv = %q (script %v), want %q", rec.argv, rec.script, want)
	}
	if rec.tmo != execTimeoutSec {
		t.Errorf("timeout = %d, want the %d default", rec.tmo, execTimeoutSec)
	}
}

func TestNodeExecUsage(t *testing.T) {
	if _, _, err := runExec(t, "n1"); err == nil || !strings.Contains(err.Error(), "usage") {
		t.Errorf("no command: err = %v, want usage", err)
	}
	if _, _, err := runExec(t, "--script", "x.sh"); err == nil || !strings.Contains(err.Error(), "usage") {
		t.Errorf("no node: err = %v, want usage", err)
	}
}

func TestReadScriptFromStdin(t *testing.T) {
	body := "#!/bin/bash\necho \"$1\"\n"
	in, err := os.Open(writeScript(t, "stdin", body))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = in.Close() }()

	s, err := readScript("-", in)
	if err != nil {
		t.Fatal(err)
	}
	if string(s.Body) != body || s.Name != "" {
		t.Errorf("got %q (name %q), want %q with no name", s.Body, s.Name, body)
	}
}

func TestReadScriptRejects(t *testing.T) {
	empty := writeScript(t, "empty.sh", " \n\t\n")
	if _, err := readScript(empty, nil); err == nil || !strings.Contains(err.Error(), "empty") {
		t.Errorf("empty script: err = %v", err)
	}
	big := writeScript(t, "big.sh", strings.Repeat("x", maxScriptBytes+1))
	if _, err := readScript(big, nil); err == nil || !strings.Contains(err.Error(), "limit") {
		t.Errorf("oversized script: err = %v", err)
	}
	if _, err := readScript(filepath.Join(t.TempDir(), "missing.sh"), nil); err == nil {
		t.Error("missing file: got nil error")
	}
}
