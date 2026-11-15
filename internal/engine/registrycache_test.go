package engine

import (
	"encoding/base64"
	"strings"
	"testing"

	"github.com/luthermonson/mayfly/internal/config"
	"github.com/luthermonson/mayfly/internal/monitoring"
	"github.com/luthermonson/mayfly/internal/registrycache"
	"github.com/luthermonson/mayfly/internal/state"
)

func cacheOn() config.RegistryCacheSpec { return config.RegistryCacheSpec{Enabled: true} }

// The mirror endpoint is derived from the utility host's recorded address —
// never guessed, never hardcoded.
func TestRegistryMirrorFromState(t *testing.T) {
	cfg := monCfg()
	cfg.RegistryCache = cacheOn()
	st := &state.Store{Monitoring: &state.Monitoring{VMID: 200, IP: "10.0.0.45"}}

	m := registryMirror(cfg, st)
	if !m.Enabled() {
		t.Fatal("mirror should be enabled once the utility host has an address")
	}
	if m.Endpoint != "http://10.0.0.45:5000" {
		t.Errorf("endpoint = %q, want http://10.0.0.45:5000", m.Endpoint)
	}
	if !m.FailOpen {
		t.Errorf("expected fail-open for a LAN mirror: %+v", m)
	}
}

func TestRegistryMirrorOffOrUnknown(t *testing.T) {
	tests := []struct {
		name  string
		cache config.RegistryCacheSpec
		st    *state.Store
	}{
		{"cache disabled", config.RegistryCacheSpec{}, &state.Store{Monitoring: &state.Monitoring{IP: "10.0.0.45"}}},
		{"no utility host in state", cacheOn(), &state.Store{}},
		{"host has no address yet", cacheOn(), &state.Store{Monitoring: &state.Monitoring{VMID: 200}}},
		{"nil state", cacheOn(), nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := monCfg()
			cfg.RegistryCache = tt.cache
			if m := registryMirror(cfg, tt.st); m.Enabled() {
				t.Errorf("expected no mirror, got %+v", m)
			}
		})
	}
}

// An explicit endpoint removes the dependency on state entirely.
func TestRegistryMirrorExplicitEndpoint(t *testing.T) {
	cfg := monCfg()
	cfg.RegistryCache = config.RegistryCacheSpec{Enabled: true, Endpoint: "https://cache.example.com"}
	m := registryMirror(cfg, &state.Store{})
	if !m.Enabled() || m.Endpoint != "https://cache.example.com" {
		t.Fatalf("explicit endpoint not used: %+v", m)
	}
}

// Enabling the cache is utility-host drift; leaving it off must hash exactly
// as it did before the feature existed.
func TestMonitoringConfigHashTracksCache(t *testing.T) {
	spec := (config.MonitoringSpec{}).Resolved()
	off := MonitoringConfigHash(spec, config.RegistryCacheSpec{})
	on := MonitoringConfigHash(spec, cacheOn())
	if off == on {
		t.Fatal("enabling the cache must change the utility-host config hash")
	}
	resized := MonitoringConfigHash(spec, config.RegistryCacheSpec{Enabled: true, MaxSizeGB: 30})
	if resized == on {
		t.Error("changing max_size_gb must change the hash")
	}
	if again := MonitoringConfigHash(spec, config.RegistryCacheSpec{}); again != off {
		t.Error("hash is not stable for a disabled cache")
	}
	// Explicit defaults must hash the same as omitted ones (no drift churn).
	if MonitoringConfigHash(spec, cacheOn().Resolved()) != on {
		t.Error("resolved and unresolved specs must hash alike")
	}
}

func TestDiffMonitoringCarriesCacheSpec(t *testing.T) {
	cfg := monCfg()
	cfg.RegistryCache = cacheOn()

	p := Diff(cfg, &state.Store{}, []byte("pem"), nil, "")
	if p.MonitoringCreate == nil || !p.MonitoringCreate.Cache.Enabled {
		t.Fatalf("create should carry the cache spec: %+v", p.MonitoringCreate)
	}
	// First apply cannot know the host's address yet — operators are told.
	if len(p.Warnings) == 0 || !strings.Contains(strings.Join(p.Warnings, "\n"), "next apply") {
		t.Errorf("expected a one-apply-lag warning, got %v", p.Warnings)
	}

	// Once the host exists with an address, no warning.
	st := &state.Store{Monitoring: &state.Monitoring{VMID: 200, IP: "10.0.0.45"}}
	if w := Diff(cfg, st, []byte("pem"), nil, "").Warnings; len(w) != 0 {
		t.Errorf("unexpected warnings once the host is known: %v", w)
	}
}

// Turning the cache on must show up as a stack-config push, not a rebuild.
func TestDiffMonitoringCacheToggleIsConfigPush(t *testing.T) {
	cfg := monCfg()
	spec := cfg.Monitoring.Resolved()
	st := &state.Store{Monitoring: &state.Monitoring{
		VMID: 200, IP: "10.0.0.45",
		Cores: spec.Cores, MemoryMB: spec.MemoryMB, DiskGB: spec.DiskGB, Template: spec.Template,
		ConfigHash: MonitoringConfigHash(spec, config.RegistryCacheSpec{}),
		TargetHash: monitoring.TargetsHash(&state.Store{}, spec.MetricsPort),
	}}
	cfg.RegistryCache = cacheOn()

	p := Diff(cfg, st, []byte("pem"), nil, "")
	u := p.MonitoringUpdate
	if u == nil || !u.PushConfig {
		t.Fatalf("expected a config push when the cache is enabled, got %+v", u)
	}
	if !u.Cache.Enabled {
		t.Error("update should carry the desired cache spec")
	}
	if u.PowerCycle || u.DiskGrowGB > 0 {
		t.Errorf("enabling the cache must not resize or reboot the host: %+v", u)
	}
}

// The cache's files are additive: disabled means byte-identical output.
func TestMonitoringFilesCacheToggle(t *testing.T) {
	e := &Engine{Cfg: monCfg(), St: &state.Store{}}
	spec := e.Cfg.Monitoring.Resolved()

	without, err := e.monitoringFiles(spec, config.RegistryCacheSpec{})
	if err != nil {
		t.Fatal(err)
	}
	with, err := e.monitoringFiles(spec, config.RegistryCacheSpec{Enabled: true, Port: 5555})
	if err != nil {
		t.Fatal(err)
	}

	paths := map[string]string{} // path → decoded content
	for _, f := range with {
		b, err := base64.StdEncoding.DecodeString(f.ContentBase64)
		if err != nil {
			t.Fatalf("%s: %v", f.Path, err)
		}
		paths[f.Path] = string(b)
	}
	for _, want := range []string{
		registrycache.ComposeDir + "/docker-compose.yml",
		registrycache.EnvPath,
		registrycache.PruneScriptPath,
		"/etc/systemd/system/" + registrycache.PruneTimer,
		"/etc/systemd/system/registry-cache-prune.service",
	} {
		if _, ok := paths[want]; !ok {
			t.Errorf("enabled cache missing file %s", want)
		}
	}
	if env := paths[registrycache.EnvPath]; !strings.Contains(env, "REGISTRY_PORT=5555") ||
		!strings.Contains(env, "REGISTRY_CACHE_MAX_GB=20") ||
		!strings.Contains(env, "REGISTRY_UPSTREAM=https://registry-1.docker.io") {
		t.Errorf("cache .env not rendered from the spec:\n%s", env)
	}

	// Disabled: none of the cache paths, and the monitoring files unchanged.
	for _, f := range without {
		if strings.Contains(f.Path, "registry") {
			t.Errorf("cache file %s written with the cache disabled", f.Path)
		}
	}
	if len(with) != len(without)+5 {
		t.Errorf("expected exactly 5 extra files with the cache on, got %d vs %d", len(with), len(without))
	}
}

// The prune script must be delivered executable even though the guest-agent
// file write carries no mode.
func TestUtilityReconcileCmd(t *testing.T) {
	on := strings.Join(utilityReconcileCmd(cacheOn()), " ")
	for _, want := range []string{
		"cd /opt/monitoring && docker compose up -d",
		"install -d -m 0755 " + registrycache.DataDir,
		"chmod 0755 " + registrycache.PruneScriptPath,
		"cd " + registrycache.ComposeDir + " && docker compose up -d",
		"systemctl enable --now " + registrycache.PruneTimer,
	} {
		if !strings.Contains(on, want) {
			t.Errorf("enable path missing %q:\n%s", want, on)
		}
	}

	off := strings.Join(utilityReconcileCmd(config.RegistryCacheSpec{}), " ")
	for _, want := range []string{
		"cd /opt/monitoring && docker compose up -d",
		"systemctl disable --now " + registrycache.PruneTimer,
		"docker compose down --remove-orphans",
		"rm -rf " + registrycache.DataDir + "/docker",
	} {
		if !strings.Contains(off, want) {
			t.Errorf("disable path missing %q:\n%s", want, off)
		}
	}
	if strings.Contains(off, "enable --now "+registrycache.PruneTimer) {
		t.Errorf("disable path still enables the timer:\n%s", off)
	}
}

func TestUtilityDirsAndUnits(t *testing.T) {
	if units := utilityUnits(config.RegistryCacheSpec{}); len(units) != 0 {
		t.Errorf("no units expected with the cache off: %v", units)
	}
	if units := utilityUnits(cacheOn()); len(units) != 1 || units[0] != registrycache.PruneTimer {
		t.Errorf("expected just the prune timer, got %v", units)
	}
	if dirs := strings.Join(utilityComposeDirs(config.RegistryCacheSpec{}), " "); strings.Contains(dirs, "registry") {
		t.Errorf("cache compose dir present with the cache off: %s", dirs)
	}
	dirs := strings.Join(utilityDirs(cacheOn()), " ")
	if !strings.Contains(dirs, registrycache.DataDir) || !strings.Contains(dirs, registrycache.ComposeDir) {
		t.Errorf("cache dirs missing: %s", dirs)
	}
}
