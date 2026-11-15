package engine

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/luthermonson/mayfly/internal/config"
	"github.com/luthermonson/mayfly/internal/ghapp"
	"github.com/luthermonson/mayfly/internal/monitoring"
	"github.com/luthermonson/mayfly/internal/provider"
	"github.com/luthermonson/mayfly/internal/state"
)

// ipInstance is a captureInstance that reports a LAN address, like a freshly
// booted linux guest.
type ipInstance struct {
	*captureInstance
	ip string
}

func (i *ipInstance) PrimaryIPv4(context.Context) (string, error) { return i.ip, nil }

// provisionProvider is a fake backend that can clone: NextID + CreateInstance
// hand out a prewired instance, and GetInstance returns it (or the monitoring
// VM's) for the later steps.
type provisionProvider struct {
	captureProvider
	nextID  int
	created provider.CreateSpec
	mon     provider.Instance
}

func (p *provisionProvider) NextID(context.Context) (int, error) { return p.nextID, nil }

func (p *provisionProvider) CreateInstance(_ context.Context, spec provider.CreateSpec) (provider.Instance, error) {
	p.created = spec
	return p.inst, nil
}

func (p *provisionProvider) GetInstance(_ context.Context, _ string, id int) (provider.Instance, error) {
	if p.mon != nil && id == 200 {
		return p.mon, nil
	}
	return p.inst, nil
}

// An apply that provisions a member must (a) log every stage through to a
// completion line, (b) end with the summary, and (c) push the monitoring
// node's scrape targets in the SAME apply, now that the member exists. Before,
// the success path printed nothing after "waiting for guest agent", and the
// target refresh only showed up on the next plan.
func TestApplyScaleUpLogsSummaryAndRefreshesTargets(t *testing.T) {
	member := &ipInstance{captureInstance: newCaptureInstance(107), ip: "10.0.2.107"}
	mon := newCaptureInstance(200)
	prov := &provisionProvider{captureProvider: captureProvider{inst: member}, nextID: 107, mon: mon}

	spec := config.PoolSpec{Hostname: "ci.example.com", Size: 1}
	cfg := testCfg(map[string]config.PoolSpec{"linux-amd64": spec})
	cfg.Monitoring = &config.MonitoringSpec{}
	monSpec := cfg.Monitoring.Resolved()

	st := &state.Store{
		Pools: []state.Pool{{Name: "linux-amd64", Hostname: "ci.example.com", Token: "tok", Secret: "sec"}},
		Monitoring: &state.Monitoring{
			VMID: 200, PVENode: "pve1",
			Cores: monSpec.Cores, MemoryMB: monSpec.MemoryMB, DiskGB: monSpec.DiskGB, Template: monSpec.Template,
			ConfigHash: MonitoringConfigHash(monSpec, config.RegistryCacheSpec{}),
		},
	}
	st.Monitoring.TargetHash = monitoring.TargetsHash(st, monSpec.MetricsPort)

	t.Setenv("HOME", t.TempDir())
	t.Setenv("USERPROFILE", t.TempDir())
	var out bytes.Buffer
	e := &Engine{
		Cfg:       cfg,
		St:        st,
		provCache: map[config.Provider]provider.Provider{config.ProviderProxmox: prov},
		GH:        &ghapp.Credentials{AppID: 1, InstallationID: 2, PrivateKeyPEM: testPEM},
		Out:       &out,
	}

	p := Diff(cfg, st, testPEM, nil, "")
	if len(p.ScaleUps) != 1 || p.MonitoringUpdate != nil {
		t.Fatalf("want a scale-up and (pre-apply) no monitoring work, got:\n%s", planText(p))
	}
	if err := e.Apply(context.Background(), p); err != nil {
		t.Fatalf("Apply: %v\n%s", err, out.String())
	}

	log := out.String()
	for _, want := range []string{
		"provisioning mfl-linux-amd64-107",
		"waiting for guest agent on mfl-linux-amd64-107",
		"guest agent up on mfl-linux-amd64-107",
		"provisioned mfl-linux-amd64-107 (vmid 107, ip 10.0.2.107)",
		"monitoring: refreshing scrape targets",
		"Apply complete: 0 added, 1 changed, 0 destroyed.",
	} {
		if !strings.Contains(log, want) {
			t.Errorf("apply output missing %q:\n%s", want, log)
		}
	}
	targets, ok := mon.writes[monitoring.TargetsPath]
	if !ok || !strings.Contains(string(targets), "10.0.2.107") {
		t.Errorf("scrape targets not pushed with the new member in the same apply; writes=%v", keys(mon.writes))
	}
	if st.Monitoring.TargetHash != monitoring.TargetsHash(st, monSpec.MetricsPort) {
		t.Error("recorded TargetHash does not match the post-apply member set")
	}
	if p := Diff(cfg, st, testPEM, nil, ""); !p.Empty() {
		t.Errorf("a second plan still has work (the first apply should have converged):\n%s", planText(p))
	}
}

// failingStart makes provisioning fail after the clone.
type failingStart struct{ *captureInstance }

func (failingStart) Start(context.Context) error { return errors.New("boom") }

// A failure mid-apply returns an error naming the step, and never prints the
// completion summary.
func TestApplyFailureHasNoSummary(t *testing.T) {
	prov := &provisionProvider{captureProvider: captureProvider{inst: failingStart{newCaptureInstance(107)}}, nextID: 107}
	cfg := testCfg(map[string]config.PoolSpec{"linux-amd64": {Hostname: "ci.example.com", Size: 1}})
	st := &state.Store{Pools: []state.Pool{{Name: "linux-amd64", Hostname: "ci.example.com"}}}
	t.Setenv("HOME", t.TempDir())
	t.Setenv("USERPROFILE", t.TempDir())
	var out bytes.Buffer
	e := &Engine{Cfg: cfg, St: st, provCache: map[config.Provider]provider.Provider{config.ProviderProxmox: prov},
		GH: &ghapp.Credentials{AppID: 1, InstallationID: 2, PrivateKeyPEM: testPEM}, Out: &out}

	err := e.Apply(context.Background(), Diff(cfg, st, testPEM, nil, ""))
	if err == nil || !strings.Contains(err.Error(), "start mfl-linux-amd64-107") {
		t.Fatalf("err = %v, want the failed start named", err)
	}
	if strings.Contains(out.String(), "Apply complete") {
		t.Errorf("failed apply printed the completion summary:\n%s", out.String())
	}
}

func planText(p *Plan) string {
	var b strings.Builder
	p.Render(&b)
	return b.String()
}
