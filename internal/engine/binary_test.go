package engine

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"

	"github.com/luthermonson/mayfly/internal/config"
	"github.com/luthermonson/mayfly/internal/ghapp"
	"github.com/luthermonson/mayfly/internal/provider"
	"github.com/luthermonson/mayfly/internal/state"
)

// binaryFleet is a one-pool fleet pinned to fromVersion, with one VM member
// recorded as running it under a current-format fingerprint.
func binaryFleet(t *testing.T, fromVersion string) (*config.Config, *state.Store, config.PoolSpec) {
	t.Helper()
	spec := config.PoolSpec{Hostname: "ci.example.com", Size: 1}
	cfg := testCfg(map[string]config.PoolSpec{"p": spec})
	cfg.Ephemerd.Version = fromVersion
	pool := statePool(cfg, "p", spec, 101)
	pool.Members[0].Ephemerd = fromVersion
	pool.Members[0].Hostname = "mfl-p-101"
	return cfg, &state.Store{Pools: []state.Pool{pool}}, spec.Resolved(cfg.Defaults)
}

func renderPlan(p *Plan) string {
	var b strings.Builder
	p.Render(&b)
	return b.String()
}

// The 2026-10-10 regression. Bumping [ephemerd] version must plan an UPGRADE
// for each member, not a config push + restart: the rendered config did not
// change, and an upgrade alone needs no webhook cordon.
func TestDiffVersionBumpIsUpgradeNotConfigRestart(t *testing.T) {
	cfg, st, _ := binaryFleet(t, "v0.2.19")
	cfg.Ephemerd.Version = "v0.2.20"

	p := Diff(cfg, st, testPEM, nil, "")
	if len(p.Updates) != 1 || len(p.Updates[0].Members) != 1 {
		t.Fatalf("want one member update, got:\n%s", renderPlan(p))
	}
	mu := p.Updates[0].Members[0]
	if mu.Config {
		t.Errorf("a version bump was planned as a config push + restart (reason %q)", mu.ConfigReason)
	}
	if !mu.Upgrade || mu.FromBinary != "v0.2.19" || mu.ToBinary != "v0.2.20" {
		t.Errorf("upgrade = %v %q → %q, want v0.2.19 → v0.2.20", mu.Upgrade, mu.FromBinary, mu.ToBinary)
	}
	if mu.restartsDaemon() {
		t.Error("a self-upgrade must not count as a daemon restart (it would cordon the pool's webhooks)")
	}
	out := renderPlan(p)
	if !strings.Contains(out, "upgrade ephemerd v0.2.19 → v0.2.20") || strings.Contains(out, "restart ephemerd") {
		t.Errorf("plan output does not describe a plain upgrade:\n%s", out)
	}
}

// Once the node is upgraded and state records it, the plan is empty: no
// spurious restart for every pool.
func TestDiffNoDriftAfterRecordedUpgrade(t *testing.T) {
	cfg, st, _ := binaryFleet(t, "v0.2.19")
	cfg.Ephemerd.Version = "v0.2.20"
	m := &st.Pools[0].Members[0]
	RecordUpgrade(cfg, st, &st.Pools[0], m, cfg.Ephemerd, "ephemerd v0.2.19", testPEM)

	if p := Diff(cfg, st, testPEM, nil, ""); !p.Empty() {
		t.Errorf("want an empty plan after a recorded upgrade, got:\n%s", renderPlan(p))
	}
}

// A real config change is still a config push + restart, and the plan says why.
func TestDiffConfigChangeExplainsReason(t *testing.T) {
	cfg, st, _ := binaryFleet(t, "v0.2.20")
	cfg.Pools["p"] = config.PoolSpec{Hostname: "ci.example.com", Size: 1, Repos: []string{"friend/project"}}

	p := Diff(cfg, st, testPEM, nil, "")
	if len(p.Updates) != 1 {
		t.Fatalf("want one update, got:\n%s", renderPlan(p))
	}
	mu := p.Updates[0].Members[0]
	if !mu.Config || mu.Upgrade {
		t.Errorf("Config=%v Upgrade=%v, want a config change only", mu.Config, mu.Upgrade)
	}
	if out := renderPlan(p); !strings.Contains(out, "update config + restart ephemerd (rendered config.toml / app.pem changed)") {
		t.Errorf("plan does not say why the member restarts:\n%s", out)
	}
}

// A per-pool ephemerd_version pin is part of the binary selection. The old
// fingerprint read only the global [ephemerd] section, so a pin change planned
// nothing at all.
func TestDiffPoolVersionPinIsUpgrade(t *testing.T) {
	cfg, st, _ := binaryFleet(t, "v0.2.19")
	cfg.Pools["p"] = config.PoolSpec{Hostname: "ci.example.com", Size: 1, EphemerdVersion: "v0.2.21"}

	p := Diff(cfg, st, testPEM, nil, "")
	if len(p.Updates) != 1 || !p.Updates[0].Members[0].Upgrade || p.Updates[0].Members[0].ToBinary != "v0.2.21" {
		t.Fatalf("want an upgrade to the pool pin v0.2.21, got:\n%s", renderPlan(p))
	}
}

// An unknown binary (state predating the field, refresh could not reach the
// node) is not drift.
func TestDiffUnknownBinaryIsNotDrift(t *testing.T) {
	cfg, st, _ := binaryFleet(t, "v0.2.19")
	st.Pools[0].Members[0].Ephemerd = ""
	cfg.Ephemerd.Version = "v0.2.20"
	if p := Diff(cfg, st, testPEM, nil, ""); !p.Empty() {
		t.Errorf("unknown binary planned work:\n%s", renderPlan(p))
	}
}

// legacyMember records m the way the old code did: a 16-hex fingerprint that
// folded in the global binary selection at the time.
func legacyMember(cfg *config.Config, spec config.PoolSpec, pool *state.Pool, m *state.Member, version string) {
	m.Ephemerd = ""
	m.ConfigHash = legacyConfigHash(cfg, nil, spec, pool, testPEM, version, "")
}

// State written by the old code keeps working: a legacy hash taken at the
// current version is recognised as converged, not as drift, so upgrading mayfly
// itself does not plan a fleet-wide restart.
func TestDiffLegacyHashAtCurrentVersionIsConverged(t *testing.T) {
	cfg, st, spec := binaryFleet(t, "v0.2.20")
	legacyMember(cfg, spec, &st.Pools[0], &st.Pools[0].Members[0], "v0.2.20")
	if p := Diff(cfg, st, testPEM, nil, ""); !p.Empty() {
		t.Errorf("legacy hash at the current version planned work:\n%s", renderPlan(p))
	}
}

// A legacy hash taken at the version the node is now known to run (the live
// probe filled Ephemerd in) separates the two drifts: config is fine, the
// binary is behind.
func TestDiffLegacyHashWithKnownOldVersionIsUpgradeOnly(t *testing.T) {
	cfg, st, spec := binaryFleet(t, "v0.2.19")
	m := &st.Pools[0].Members[0]
	legacyMember(cfg, spec, &st.Pools[0], m, "v0.2.19")
	m.Ephemerd = "v0.2.19" // as refreshMemberVersions would set it
	cfg.Ephemerd.Version = "v0.2.20"

	p := Diff(cfg, st, testPEM, nil, "")
	if len(p.Updates) != 1 {
		t.Fatalf("want one update, got:\n%s", renderPlan(p))
	}
	if mu := p.Updates[0].Members[0]; mu.Config || !mu.Upgrade {
		t.Errorf("Config=%v (%q) Upgrade=%v, want upgrade only", mu.Config, mu.ConfigReason, mu.Upgrade)
	}
}

// A legacy hash that matches no known binary cannot be split; it is still
// flagged, but the plan says why and how to clear it without a restart.
func TestDiffUnresolvableLegacyHashExplainsItself(t *testing.T) {
	cfg, st, spec := binaryFleet(t, "v0.2.19")
	m := &st.Pools[0].Members[0]
	legacyMember(cfg, spec, &st.Pools[0], m, "v0.2.19")
	cfg.Ephemerd.Version = "v0.2.20"
	m.Ephemerd = "v0.2.20" // upgraded outside mayfly; the old version is unknown

	p := Diff(cfg, st, testPEM, nil, "")
	if len(p.Updates) != 1 || !p.Updates[0].Members[0].Config {
		t.Fatalf("want a config flag, got:\n%s", renderPlan(p))
	}
	if out := renderPlan(p); !strings.Contains(out, "predates separate binary tracking") || !strings.Contains(out, "reconfigure --no-restart") {
		t.Errorf("plan does not explain the legacy fingerprint:\n%s", out)
	}
}

// RecordUpgrade migrates a legacy hash using the version the node upgraded
// FROM, which is exactly the binary the old fingerprint was taken with.
func TestRecordUpgradeMigratesLegacyHash(t *testing.T) {
	cfg, st, spec := binaryFleet(t, "v0.2.19")
	m := &st.Pools[0].Members[0]
	legacyMember(cfg, spec, &st.Pools[0], m, "v0.2.19")
	cfg.Ephemerd.Version = "v0.2.20"

	RecordUpgrade(cfg, st, &st.Pools[0], m, cfg.Ephemerd, "ephemerd version 0.2.19 (abc1234)", testPEM)
	if !strings.HasPrefix(m.ConfigHash, configHashPrefix) {
		t.Errorf("legacy hash not migrated: %q", m.ConfigHash)
	}
	if m.Ephemerd != "v0.2.20" {
		t.Errorf("Ephemerd = %q, want v0.2.20", m.Ephemerd)
	}
	if p := Diff(cfg, st, testPEM, nil, ""); !p.Empty() {
		t.Errorf("want an empty plan, got:\n%s", renderPlan(p))
	}
}

func TestAdoptLiveVersion(t *testing.T) {
	tests := []struct {
		name, recorded, reported, want string
		wantRef                        string
		changed                        bool
	}{
		{"fills unknown", "", "v0.2.20", "v0.2.20", "v0.2.20", true},
		{"corrects out-of-band upgrade", "v0.2.19", "ephemerd version 0.2.20", "v0.2.20", "v0.2.20", true},
		{"same version is no change", "v0.2.20", "0.2.20", "v0.2.20", "v0.2.20", false},
		{"unparseable report keeps state", "v0.2.19", "garbage", "v0.2.20", "v0.2.19", false},
		{"pinned url cannot be confirmed", "url:https://x/e.tgz", "v0.2.20", "url:https://x/e.tgz", "url:https://x/e.tgz", false},
		{"url no longer pinned is replaced", "url:https://x/e.tgz", "v0.2.20", "v0.2.20", "v0.2.20", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := &state.Member{Ephemerd: tt.recorded}
			_, changed := adoptLiveVersion(m, tt.reported, tt.want)
			if changed != tt.changed || m.Ephemerd != tt.wantRef {
				t.Errorf("changed=%v Ephemerd=%q, want changed=%v Ephemerd=%q", changed, m.Ephemerd, tt.changed, tt.wantRef)
			}
		})
	}
}

// upgradeInstance answers like a real node across a self-upgrade: --version
// reports from, the upgrade command flips it, and status reports the current
// version as serving.
type upgradeInstance struct {
	*captureInstance
	version string
	to      string
}

func (u *upgradeInstance) Exec(ctx context.Context, argv []string, t int) (provider.ExecStatus, error) {
	_, _ = u.captureInstance.Exec(ctx, argv, t)
	switch {
	case containsWord(argv, "--version") && !containsWord(argv, "upgrade"):
		return provider.ExecStatus{OutData: "ephemerd " + u.version}, nil
	case containsWord(argv, "upgrade"):
		u.version = u.to
		return provider.ExecStatus{}, nil
	case containsWord(argv, "status"):
		return provider.ExecStatus{OutData: `{"status":"ok","version":"` + u.version + `","draining":false,"active_jobs":0}`}, nil
	}
	return provider.ExecStatus{}, nil
}

// Apply's upgrade tier drives the daemon's self-upgrade, verifies it against
// the node, records the binary, and does NOT restart ephemerd by hand.
func TestUpdateMemberUpgradeOnly(t *testing.T) {
	inst := &upgradeInstance{captureInstance: newCaptureInstance(101), version: "v0.2.19", to: "v0.2.20"}
	cfg, st, spec := binaryFleet(t, "v0.2.19")
	cfg.Ephemerd.Version = "v0.2.20"
	var out bytes.Buffer
	e := &Engine{
		Cfg:       cfg,
		St:        st,
		provCache: map[config.Provider]provider.Provider{config.ProviderProxmox: &captureProvider{inst: inst}},
		GH:        &ghapp.Credentials{PrivateKeyPEM: testPEM},
		Out:       &out,
	}
	p := Diff(cfg, st, testPEM, nil, "")
	if len(p.Updates) != 1 {
		t.Fatalf("want one update, got:\n%s", renderPlan(p))
	}
	if err := e.updateMember(context.Background(), &st.Pools[0], spec, p.Updates[0].Members[0]); err != nil {
		t.Fatalf("updateMember: %v\n%s", err, out.String())
	}
	for _, argv := range inst.execs {
		if containsWord(argv, "restart") {
			t.Errorf("upgrade-only update ran a restart: %v", argv)
		}
	}
	if len(inst.writes) != 0 {
		t.Errorf("upgrade-only update wrote config files: %v", keys(inst.writes))
	}
	if got := st.Pools[0].Members[0].Ephemerd; got != "v0.2.20" {
		t.Errorf("recorded Ephemerd = %q, want v0.2.20", got)
	}
	if p := Diff(cfg, st, testPEM, nil, ""); !p.Empty() {
		t.Errorf("plan not empty after the upgrade:\n%s", renderPlan(p))
	}
}

// refreshMemberVersions reads the live version and corrects state, reporting
// an out-of-band upgrade as a finding rather than leaving it to plan as drift.
func TestRefreshMemberVersionsCorrectsState(t *testing.T) {
	inst := &upgradeInstance{captureInstance: newCaptureInstance(101), version: "v0.2.20"}
	cfg, st, _ := binaryFleet(t, "v0.2.19")
	cfg.Ephemerd.Version = "v0.2.20"
	e := &Engine{
		Cfg:       cfg,
		St:        st,
		provCache: map[config.Provider]provider.Provider{config.ProviderProxmox: &captureProvider{inst: inst}},
		Out:       io.Discard,
	}
	res := &RefreshResult{}
	e.refreshMemberVersions(context.Background(), res)

	if got := st.Pools[0].Members[0].Ephemerd; got != "v0.2.20" {
		t.Errorf("Ephemerd = %q after refresh, want the live v0.2.20", got)
	}
	if len(res.Findings) != 1 || res.Findings[0].Kind != "ephemerd-version" {
		t.Errorf("findings = %+v, want one ephemerd-version correction", res.Findings)
	}
	if p := Diff(cfg, st, testPEM, nil, ""); !p.Empty() {
		t.Errorf("plan not empty once the live version is known:\n%s", renderPlan(p))
	}
}
