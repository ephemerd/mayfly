package engine

import (
	"context"
	"io"
	"strings"
	"testing"

	"github.com/luthermonson/mayfly/internal/config"
	"github.com/luthermonson/mayfly/internal/ghapp"
	"github.com/luthermonson/mayfly/internal/provider"
	"github.com/luthermonson/mayfly/internal/state"
)

// captureInstance is a provider.Instance that records the FileWrites and Execs
// the engine performs on it, so a reconfigure can be asserted without a live
// hypervisor. Everything else the interface requires is a no-op.
type captureInstance struct {
	id     int
	writes map[string][]byte
	execs  [][]string
}

func newCaptureInstance(id int) *captureInstance {
	return &captureInstance{id: id, writes: map[string][]byte{}}
}

func (c *captureInstance) ID() int                                      { return c.id }
func (c *captureInstance) SetResources(context.Context, int, int) error { return nil }
func (c *captureInstance) ResizeDisk(context.Context, int) error        { return nil }
func (c *captureInstance) MoveDisk(context.Context, string) error      { return nil }
func (c *captureInstance) DiskStorage(context.Context) (string, string, error) {
	return "local", "", nil
}
func (c *captureInstance) Start(context.Context) error                  { return nil }
func (c *captureInstance) Shutdown(context.Context) error               { return nil }
func (c *captureInstance) Stop(context.Context) error                   { return nil }
func (c *captureInstance) DiskGB(context.Context) (int, error)          { return 0, nil }
func (c *captureInstance) IsRunning(context.Context) (bool, error)      { return true, nil }
func (c *captureInstance) WaitForReady(context.Context, int) error      { return nil }
func (c *captureInstance) PrimaryIPv4(context.Context) (string, error)  { return "", nil }
func (c *captureInstance) Delete(context.Context) error                 { return nil }

func (c *captureInstance) Exec(_ context.Context, argv []string, _ int) (provider.ExecStatus, error) {
	c.execs = append(c.execs, argv)
	return provider.ExecStatus{ExitCode: 0}, nil
}

func (c *captureInstance) FileWrite(_ context.Context, path string, content []byte) error {
	c.writes[path] = append([]byte(nil), content...)
	return nil
}

// captureProvider hands back one prewired instance and records the (node, id)
// GetInstance was asked for. Only GetInstance is exercised; the embedded
// interface supplies the rest (and would panic if the engine reached for them,
// which it must not on the reconfigure path).
type captureProvider struct {
	provider.Provider
	inst    provider.Instance
	gotNode string
	gotID   int
}

func (p *captureProvider) GetInstance(_ context.Context, node string, id int) (provider.Instance, error) {
	p.gotNode, p.gotID = node, id
	return p.inst, nil
}

// reconfEngine builds an Engine wired to a fake Proxmox backend and a one-pool,
// one-VM-member state, enough to drive ReconfigureMember end to end.
func reconfEngine(inst provider.Instance) (*Engine, *captureProvider, *state.Member) {
	fp := &captureProvider{inst: inst}
	cfg := &config.Config{
		GitHub: config.GitHub{Owner: "acme"},
		Pools: map[string]config.PoolSpec{
			"linux": {Hostname: "ci.example.com", Size: 1, Repos: []string{"repo1"}, MaxConcurrent: 4},
		},
	}
	st := &state.Store{
		Pools: []state.Pool{{
			Name:     "linux",
			Hostname: "ci.example.com",
			Token:    "tok",
			Secret:   "shh",
			Members: []state.Member{{
				VMID: 101, Hostname: "mfl-linux-101", PVENode: "node-a", OS: "linux", Arch: "amd64",
			}},
		}},
	}
	e := &Engine{
		Cfg:       cfg,
		St:        st,
		provCache: map[config.Provider]provider.Provider{config.ProviderProxmox: fp},
		GH:        &ghapp.Credentials{AppID: 111, InstallationID: 222, PrivateKeyPEM: []byte("PEM-BYTES")},
		Out:       io.Discard,
	}
	return e, fp, &st.Pools[0].Members[0]
}

// A restart-mode reconfigure renders the pool config, writes config.toml +
// app.pem to the right guest paths, and bounces ephemerd — and it resolves the
// instance from the member's recorded node/vmid.
func TestReconfigureMemberVMWithRestart(t *testing.T) {
	inst := newCaptureInstance(101)
	e, fp, member := reconfEngine(inst)
	before := member.ConfigHash

	if err := e.ReconfigureMember(context.Background(), "linux", member, true); err != nil {
		t.Fatalf("ReconfigureMember: %v", err)
	}

	if fp.gotNode != "node-a" || fp.gotID != 101 {
		t.Errorf("GetInstance(node=%q,id=%d), want (node-a,101)", fp.gotNode, fp.gotID)
	}

	cfgBytes, ok := inst.writes[configPath("linux")]
	if !ok {
		t.Fatalf("no config.toml written to %s; writes=%v", configPath("linux"), keys(inst.writes))
	}
	got := string(cfgBytes)
	for _, want := range []string{"[github]", `owner = "acme"`, `repos = ["repo1"]`, "max_concurrent = 4", `cloudflared_token = "tok"`, `secret = "shh"`} {
		if !strings.Contains(got, want) {
			t.Errorf("rendered config missing %q:\n%s", want, got)
		}
	}

	pem, ok := inst.writes[pemPath("linux")]
	if !ok {
		t.Fatalf("no app.pem written to %s", pemPath("linux"))
	}
	if string(pem) != "PEM-BYTES" {
		t.Errorf("app.pem = %q, want the GitHub App PEM", pem)
	}

	if len(inst.execs) != 1 {
		t.Fatalf("got %d execs, want exactly the restart", len(inst.execs))
	}
	if !containsWord(inst.execs[0], "drain") || !containsWord(inst.execs[0], "restart") {
		t.Errorf("restart exec = %v, want the drain+restart command", inst.execs[0])
	}

	if member.ConfigHash == "" || member.ConfigHash == before {
		t.Errorf("ConfigHash not refreshed (before=%q after=%q)", before, member.ConfigHash)
	}
}

// --no-restart stages the config: files land, but ephemerd is left alone.
func TestReconfigureMemberVMNoRestart(t *testing.T) {
	inst := newCaptureInstance(101)
	e, _, member := reconfEngine(inst)

	if err := e.ReconfigureMember(context.Background(), "linux", member, false); err != nil {
		t.Fatalf("ReconfigureMember: %v", err)
	}

	if _, ok := inst.writes[configPath("linux")]; !ok {
		t.Errorf("config.toml not written even in stage-only mode")
	}
	if _, ok := inst.writes[pemPath("linux")]; !ok {
		t.Errorf("app.pem not written even in stage-only mode")
	}
	if len(inst.execs) != 0 {
		t.Errorf("got %d execs with --no-restart, want none: %v", len(inst.execs), inst.execs)
	}
}

// A member whose pool is no longer in config cannot be re-rendered — there is no
// spec to render from — and the command must say so rather than push a stale or
// empty config.
func TestReconfigureMemberPoolNotInConfig(t *testing.T) {
	inst := newCaptureInstance(101)
	e, _, member := reconfEngine(inst)
	delete(e.Cfg.Pools, "linux")

	err := e.ReconfigureMember(context.Background(), "linux", member, true)
	if err == nil {
		t.Fatal("want an error when the pool is absent from config")
	}
	if !strings.Contains(err.Error(), "not in config") {
		t.Errorf("error = %v, want it to explain the pool is not in config", err)
	}
	if len(inst.writes) != 0 || len(inst.execs) != 0 {
		t.Errorf("nothing should have been pushed; writes=%v execs=%v", keys(inst.writes), inst.execs)
	}
}

func keys(m map[string][]byte) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func containsWord(argv []string, word string) bool {
	for _, a := range argv {
		if strings.Contains(a, word) {
			return true
		}
	}
	return false
}
