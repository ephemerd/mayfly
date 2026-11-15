package engine

import (
	"strings"
	"testing"

	"github.com/luthermonson/mayfly/internal/cloudinit"
	"github.com/luthermonson/mayfly/internal/config"
	"github.com/luthermonson/mayfly/internal/registrycache"
)

// TestGuestParentDirs_DedupesAndSorts pins the shape updateMonitoring depends
// on: one entry per directory, deterministic order, and no bare "/" or "."
// which install -d would either no-op on or create in the wrong place.
func TestGuestParentDirs_DedupesAndSorts(t *testing.T) {
	files := []cloudinit.WriteFile{
		{Path: "/opt/registry-cache/docker-compose.yml"},
		{Path: "/opt/registry-cache/.env"},
		{Path: "/etc/systemd/system/registry-cache-prune.service"},
		{Path: "/etc/systemd/system/registry-cache-prune.timer"},
		{Path: "/toplevel"},
		{Path: ""},
	}
	got := guestParentDirs(files)
	want := []string{"/etc/systemd/system", "/opt/registry-cache"}
	if len(got) != len(want) {
		t.Fatalf("guestParentDirs() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("guestParentDirs()[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

// TestGuestParentDirs_PosixOnHost is the regression that matters when mayfly
// runs from Windows: guest paths are POSIX, so splitting them with
// filepath.Dir would return "." for every entry and create nothing.
func TestGuestParentDirs_PosixOnHost(t *testing.T) {
	got := guestParentDirs([]cloudinit.WriteFile{
		{Path: "/opt/registry-cache/docker-compose.yml"},
	})
	if len(got) != 1 || got[0] != "/opt/registry-cache" {
		t.Fatalf("guestParentDirs() = %v, want [/opt/registry-cache]", got)
	}
}

// TestMkdirCmd_CoversRegistryCacheDir is the actual bug: enabling the registry
// cache on a utility host that predates it wrote docker-compose.yml into a
// directory nothing had created, and apply failed with "No such file or
// directory". The create path never saw it because cloud-init's write_files
// makes parents implicitly.
func TestMkdirCmd_CoversRegistryCacheDir(t *testing.T) {
	files, err := registryCacheFiles(config.RegistryCacheSpec{Enabled: true})
	if err != nil {
		t.Fatalf("registryCacheFiles() error: %v", err)
	}
	if len(files) == 0 {
		t.Fatal("registryCacheFiles() returned nothing with the cache enabled")
	}
	cmd := mkdirCmd(guestParentDirs(files))
	joined := strings.Join(cmd, " ")
	if !strings.Contains(joined, registrycache.ComposeDir) {
		t.Errorf("mkdir command does not create %s: %q", registrycache.ComposeDir, joined)
	}
	if !strings.Contains(joined, "set -e") {
		t.Errorf("mkdir command should fail fast, got %q", joined)
	}
}

// TestMkdirCmd_EmptyIsNoop keeps callers from needing a guard.
func TestMkdirCmd_EmptyIsNoop(t *testing.T) {
	cmd := mkdirCmd(nil)
	if len(cmd) != 3 || cmd[2] != "true" {
		t.Errorf("mkdirCmd(nil) = %v, want a no-op", cmd)
	}
}
