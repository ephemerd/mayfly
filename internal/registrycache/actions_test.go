package registrycache

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/crypto/bcrypt"
)

func TestActionsStackFilesLayout(t *testing.T) {
	files, err := ActionsStackFiles()
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		ActionsComposeDir + "/docker-compose.yml":      "0644",
		ActionsGCScriptPath:                            "0755",
		"/etc/systemd/system/actions-cache-gc.service": "0644",
		"/etc/systemd/system/" + ActionsGCTimer:        "0644",
	}
	if len(files) != len(want) {
		t.Fatalf("got %d files, want %d: %+v", len(files), len(want), files)
	}
	for _, f := range files {
		if mode, ok := want[f.Path]; !ok || mode != f.Mode {
			t.Errorf("unexpected file %s (mode %s)", f.Path, f.Mode)
		}
	}
}

// The pull-through stack must not pick the Actions assets up: they share an
// embed tree, and a utility host or a cache VM without [registry_cache.actions]
// must keep exactly the files (and AssetsHash) it had before.
func TestStackFilesExcludeActionsAssets(t *testing.T) {
	files, err := StackFiles()
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		if strings.Contains(f.Path, "actions-cache") {
			t.Errorf("pull-through stack includes an Actions asset: %s", f.Path)
		}
	}
}

// The compose file is the security boundary: writable, authenticated, deletes
// on (eviction), and never a proxy (a proxy refuses pushes).
func TestActionsComposeIsAuthenticatedAndWritable(t *testing.T) {
	files, err := ActionsStackFiles()
	if err != nil {
		t.Fatal(err)
	}
	var compose string
	for _, f := range files {
		if strings.HasSuffix(f.Path, "docker-compose.yml") {
			compose = string(f.Content)
		}
	}
	for _, want := range []string{
		"REGISTRY_AUTH: htpasswd",
		"REGISTRY_AUTH_HTPASSWD_PATH: /auth/htpasswd",
		`REGISTRY_STORAGE_DELETE_ENABLED: "true"`,
		ActionsDataDir + ":/var/lib/registry",
		ActionsHtpasswdPath + ":/auth/htpasswd:ro",
	} {
		if !strings.Contains(compose, want) {
			t.Errorf("compose missing %q", want)
		}
	}
	if strings.Contains(compose, "REGISTRY_PROXY_REMOTEURL") {
		t.Error("Actions registry is configured as a proxy; it would refuse every push")
	}
}

func TestActionsHtpasswdIsBcrypt(t *testing.T) {
	b, err := ActionsHtpasswd("ephemerd", "s3cret")
	if err != nil {
		t.Fatal(err)
	}
	line := strings.TrimSpace(string(b))
	user, hash, ok := strings.Cut(line, ":")
	if !ok || user != "ephemerd" {
		t.Fatalf("htpasswd line = %q", line)
	}
	if !strings.HasPrefix(hash, "$2") {
		t.Fatalf("hash %q is not bcrypt; registry:2 accepts only bcrypt", hash)
	}
	if err := bcrypt.CompareHashAndPassword([]byte(hash), []byte("s3cret")); err != nil {
		t.Fatalf("hash does not verify the password: %v", err)
	}
	if strings.Contains(string(b), "s3cret") {
		t.Fatal("plaintext password leaked into htpasswd")
	}
	if _, err := ActionsHtpasswd("ephemerd", ""); err == nil {
		t.Fatal("empty password accepted")
	}
}

// bcrypt salts, so the htpasswd bytes are not stable; the fingerprint stands in
// for the credential in config hashes and must be stable but change with it.
func TestActionsCredentialFingerprint(t *testing.T) {
	a := ActionsCredentialFingerprint("ephemerd", "one")
	if a != ActionsCredentialFingerprint("ephemerd", "one") {
		t.Fatal("fingerprint not stable")
	}
	if a == ActionsCredentialFingerprint("ephemerd", "two") {
		t.Fatal("fingerprint ignores the password; a rotation would not re-push")
	}
	if strings.Contains(a, "one") {
		t.Fatal("fingerprint contains the plaintext")
	}
}

func TestActionsEnvFileHasNoSecret(t *testing.T) {
	env := string(ActionsEnvFile(ActionsOptions{Port: 5001, MaxSizeGB: 150}))
	for _, want := range []string{"ACTIONS_REGISTRY_PORT=5001", "ACTIONS_CACHE_MAX_GB=150", "ACTIONS_CACHE_DATA=" + ActionsDataDir} {
		if !strings.Contains(env, want) {
			t.Errorf(".env missing %q:\n%s", want, env)
		}
	}
	if strings.Contains(strings.ToLower(env), "password") {
		t.Errorf(".env mentions a password:\n%s", env)
	}
}

// runGC runs the real gc.sh with a fake `docker` (and `logger`) on PATH that
// records each call and fails the named subcommand. It returns the recorded
// calls and the script's exit error.
func runGC(t *testing.T, failOn string) ([]string, error) {
	t.Helper()
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh on PATH")
	}
	files, err := ActionsStackFiles()
	if err != nil {
		t.Fatal(err)
	}
	var script []byte
	for _, f := range files {
		if f.Path == ActionsGCScriptPath {
			script = f.Content
		}
	}
	dir := t.TempDir()
	bin, project, data := filepath.Join(dir, "bin"), filepath.Join(dir, "project"), filepath.Join(dir, "data")
	for _, d := range []string{bin, project, data} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(project, "docker-compose.yml"), []byte("services: {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	calls := filepath.Join(dir, "calls")
	fake := "#!/bin/sh\necho \"$2\" >> '" + filepath.ToSlash(calls) + "'\n" +
		"[ \"$2\" = '" + failOn + "' ] && exit 7\nexit 0\n"
	for name, body := range map[string]string{"docker": fake, "logger": "#!/bin/sh\nexit 0\n"} {
		if err := os.WriteFile(filepath.Join(bin, name), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	gc := filepath.Join(dir, "gc.sh")
	if err := os.WriteFile(gc, script, 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(sh, gc)
	cmd.Env = append(os.Environ(),
		"PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"),
		"ACTIONS_CACHE_DIR="+project, "ACTIONS_CACHE_DATA="+data, "ACTIONS_CACHE_MAX_GB=150")
	runErr := cmd.Run()
	b, _ := os.ReadFile(calls)
	return strings.Fields(string(b)), runErr
}

// The gc stops the registry; whatever fails afterwards, it must be started
// again or the Actions cache stays down until next week's run.
func TestActionsGCAlwaysRestartsRegistry(t *testing.T) {
	for _, tc := range []struct {
		failOn  string
		wantErr bool
	}{
		{"", false},    // normal run
		{"run", false}, // garbage-collect fails: logged, registry restarted
		{"stop", true}, // stop fails under set -e: trap still brings it up
	} {
		t.Run("fail="+tc.failOn, func(t *testing.T) {
			calls, err := runGC(t, tc.failOn)
			if (err != nil) != tc.wantErr {
				t.Fatalf("exit err = %v, want error %v (calls %v)", err, tc.wantErr, calls)
			}
			if len(calls) == 0 || calls[len(calls)-1] != "up" {
				t.Fatalf("registry not restarted last: calls %v", calls)
			}
			ups := 0
			for _, c := range calls {
				if c == "up" {
					ups++
				}
			}
			if ups != 1 {
				t.Fatalf("restarted %d times, want once: %v", ups, calls)
			}
		})
	}
}
