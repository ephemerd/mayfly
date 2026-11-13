package registrycache

import (
	"strings"
	"testing"
)

func TestStackFilesGuestLayout(t *testing.T) {
	files, err := StackFiles()
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{ // path → mode
		ComposeDir + "/docker-compose.yml":                 "0644",
		PruneScriptPath:                                    "0755",
		"/etc/systemd/system/registry-cache-prune.service": "0644",
		"/etc/systemd/system/" + PruneTimer:                "0644",
	}
	got := map[string]string{}
	for _, f := range files {
		got[f.Path] = f.Mode
		if len(f.Content) == 0 {
			t.Errorf("%s is empty", f.Path)
		}
	}
	if len(got) != len(want) {
		t.Fatalf("got %d guest files, want %d: %v", len(got), len(want), got)
	}
	for path, mode := range want {
		if got[path] != mode {
			t.Errorf("%s mode = %q, want %q", path, got[path], mode)
		}
	}
	// The README documents the layout; it must never land on the VM.
	for _, f := range files {
		if strings.HasSuffix(f.Path, "README.md") {
			t.Errorf("README shipped to the guest: %s", f.Path)
		}
	}
}

// The compose file must run the registry in proxy mode with deletes on —
// without either, it is not a bounded pull-through cache.
func TestComposeIsBoundedProxy(t *testing.T) {
	files, err := StackFiles()
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
		"REGISTRY_PROXY_REMOTEURL",
		"REGISTRY_PROXY_TTL",
		`REGISTRY_STORAGE_DELETE_ENABLED: "true"`,
		"restart: unless-stopped", // survives reboots
		DataDir + ":/var/lib/registry",
		"${REGISTRY_PORT:-5000}:5000", // LAN-published
	} {
		if !strings.Contains(compose, want) {
			t.Errorf("compose missing %q:\n%s", want, compose)
		}
	}
}

func TestEnvFile(t *testing.T) {
	env := string(EnvFile(Options{
		Port: 5555, Upstream: "https://ghcr.io", TTL: "24h",
		MaxSizeGB: 30, Username: "bot", Password: "s3cret",
	}))
	for _, want := range []string{
		"REGISTRY_PORT=5555",
		"REGISTRY_UPSTREAM=https://ghcr.io",
		"REGISTRY_TTL=24h",
		"REGISTRY_CACHE_MAX_GB=30",
		"REGISTRY_UPSTREAM_USERNAME=bot",
		"REGISTRY_UPSTREAM_PASSWORD=s3cret",
		"REGISTRY_CACHE_DATA=" + DataDir,
		"REGISTRY_CACHE_DIR=" + ComposeDir,
	} {
		if !strings.Contains(env, want) {
			t.Errorf("env missing %q:\n%s", want, env)
		}
	}
	// Every line must be a systemd-parsable KEY=VALUE (the prune unit loads
	// this file as its EnvironmentFile).
	for _, line := range strings.Split(strings.TrimSpace(env), "\n") {
		if !strings.Contains(line, "=") || strings.HasPrefix(line, "=") {
			t.Errorf("not a KEY=VALUE line: %q", line)
		}
	}
}

func TestAssetsHashStable(t *testing.T) {
	h := AssetsHash()
	if len(h) != 16 {
		t.Fatalf("hash = %q, want 16 hex chars", h)
	}
	if h != AssetsHash() {
		t.Error("AssetsHash is not deterministic")
	}
	if strings.HasPrefix(h, "invalid:") {
		t.Errorf("assets failed to load: %s", h)
	}
}

func TestStackDirs(t *testing.T) {
	dirs := StackDirs()
	if len(dirs) != 2 || dirs[0] != ComposeDir || dirs[1] != DataDir {
		t.Errorf("StackDirs() = %v, want [%s %s]", dirs, ComposeDir, DataDir)
	}
}
