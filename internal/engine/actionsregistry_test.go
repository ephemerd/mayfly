package engine

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/ephemerd/mayfly/internal/config"
	"github.com/ephemerd/mayfly/internal/provider"
	"github.com/ephemerd/mayfly/internal/registrycache"
	"github.com/ephemerd/mayfly/internal/state"
)

// actionsCacheCfg is dedicatedCacheCfg with the writable Actions registry on.
func actionsCacheCfg() *config.Config {
	cfg := dedicatedCacheCfg()
	cfg.RegistryCache.VM.DiskGB = 200
	cfg.RegistryCache.Actions = &config.ActionsRegistrySpec{Enabled: true, Password: "s3cret"}
	return cfg
}

// A cache VM without the Actions registry must keep the hash it had before the
// feature existed, whether the block is absent or present-but-disabled — or
// every converged cache VM re-pushes on the first apply after upgrading.
func TestRegistryCacheConfigHash_ActionsOffIsUnchanged(t *testing.T) {
	base := dedicatedCacheCfg().RegistryCache
	off := base
	off.Actions = &config.ActionsRegistrySpec{Enabled: false, Password: "x"}
	if RegistryCacheConfigHash(base) != RegistryCacheConfigHash(off) {
		t.Fatal("a disabled actions block changed the cache VM's hash")
	}
	if RegistryCacheConfigHash(base) == RegistryCacheConfigHash(actionsCacheCfg().RegistryCache) {
		t.Fatal("enabling the Actions registry did not change the hash; it would never be deployed")
	}
}

// Rotating the password must re-push (new htpasswd) without the plaintext
// being what is hashed.
func TestRegistryCacheConfigHash_PasswordRotationRepushes(t *testing.T) {
	a := actionsCacheCfg().RegistryCache
	b := actionsCacheCfg().RegistryCache
	b.Actions.Password = "rotated"
	if RegistryCacheConfigHash(a) == RegistryCacheConfigHash(b) {
		t.Fatal("a password rotation did not change the hash")
	}
}

func filesByPath(t *testing.T, cache config.RegistryCacheSpec) map[string][]byte {
	t.Helper()
	files, err := registryCacheVMFiles(cache)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string][]byte{}
	for _, f := range files {
		b, err := base64.StdEncoding.DecodeString(f.ContentBase64)
		if err != nil {
			t.Fatal(err)
		}
		out[f.Path] = b
	}
	return out
}

func TestRegistryCacheVMFiles_ActionsRegistry(t *testing.T) {
	on := filesByPath(t, actionsCacheCfg().RegistryCache)
	for _, p := range []string{
		registrycache.ActionsComposeDir + "/docker-compose.yml",
		registrycache.ActionsEnvPath,
		registrycache.ActionsHtpasswdPath,
		registrycache.ActionsGCScriptPath,
		registrycache.ComposeDir + "/docker-compose.yml", // the pull-through cache stays
	} {
		if _, ok := on[p]; !ok {
			t.Errorf("missing %s", p)
		}
	}
	if !bytes.HasPrefix(on[registrycache.ActionsHtpasswdPath], []byte("ephemerd:$2")) {
		t.Errorf("htpasswd is not a bcrypt entry for ephemerd: %q", on[registrycache.ActionsHtpasswdPath])
	}
	for p, b := range on {
		if bytes.Contains(b, []byte("s3cret")) {
			t.Errorf("plaintext password written to %s", p)
		}
	}

	off := filesByPath(t, dedicatedCacheCfg().RegistryCache)
	for p := range off {
		if strings.HasPrefix(p, registrycache.ActionsComposeDir) {
			t.Errorf("Actions file %s rendered with the Actions registry off", p)
		}
	}
}

// The dedicated VM's reconcile must not touch the monitoring project (there is
// none on the cache VM), and must bring the Actions project up or down.
func TestRegistryCacheVMReconcileCmd(t *testing.T) {
	on := strings.Join(registryCacheVMReconcileCmd(actionsCacheCfg().RegistryCache), " ")
	for _, want := range []string{
		"cd " + registrycache.ComposeDir + " && docker compose up -d",
		"cd " + registrycache.ActionsComposeDir + " && docker compose up -d --force-recreate",
		"systemctl enable --now " + registrycache.ActionsGCTimer,
	} {
		if !strings.Contains(on, want) {
			t.Errorf("reconcile missing %q:\n%s", want, on)
		}
	}
	if strings.Contains(on, "/opt/monitoring") {
		t.Errorf("cache VM reconcile reaches for the monitoring project:\n%s", on)
	}
	off := strings.Join(registryCacheVMReconcileCmd(dedicatedCacheCfg().RegistryCache), " ")
	if !strings.Contains(off, "systemctl disable --now "+registrycache.ActionsGCTimer) ||
		!strings.Contains(off, "docker compose down") {
		t.Errorf("turning the Actions registry off does not stop it:\n%s", off)
	}
}

// vm_storage on an existing cache VM is a LIVE disk move, never a recreate: the
// VM, its address (which every runner's config embeds) and its blobs survive.
func TestDiffRegistryCacheVM_VMStorageMovesLive(t *testing.T) {
	cfg := dedicatedCacheCfg()
	cfg.RegistryCache.VM.VMStorage = "local-lvm"
	cfg.RegistryCache.VM.DiskGB = 200
	st := &state.Store{RegistryCache: &state.RegistryCache{
		VMID: 101, IP: "192.0.2.224", Cores: 2, MemoryMB: 2048, DiskGB: 32,
		ConfigHash: RegistryCacheConfigHash(cfg.RegistryCache),
	}}
	p := &Plan{}
	diffRegistryCacheVM(cfg, st, p)
	if p.RegistryCacheCreate != nil || p.RegistryCacheDestroy {
		t.Fatal("a storage change planned a recreate; it must be a live move")
	}
	u := p.RegistryCacheUpdate
	if u == nil || u.MoveDiskTo != "local-lvm" || u.DiskGrowGB != 200 {
		t.Fatalf("update = %+v, want a move to local-lvm and a grow to 200G", u)
	}
	var out bytes.Buffer
	p.Render(&out)
	for _, want := range []string{"move disk clone storage→local-lvm (live, VM and IP kept)", "grow disk 32G→200G (live)"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("plan output missing %q:\n%s", want, out.String())
		}
	}

	// Converged: recorded storage matches, nothing to do.
	st.RegistryCache.VMStorage, st.RegistryCache.DiskGB = "local-lvm", 200
	p = &Plan{}
	diffRegistryCacheVM(cfg, st, p)
	if p.RegistryCacheUpdate != nil {
		t.Fatalf("converged storage still plans work: %+v", p.RegistryCacheUpdate)
	}

	// Clearing vm_storage leaves the disk where it is rather than moving it
	// back onto the small template storage.
	cfg.RegistryCache.VM.VMStorage = ""
	p = &Plan{}
	diffRegistryCacheVM(cfg, st, p)
	if p.RegistryCacheUpdate != nil && p.RegistryCacheUpdate.MoveDiskTo != "" {
		t.Fatal("clearing vm_storage planned a move")
	}
}

func TestActionsCacheRenderInput(t *testing.T) {
	cfg := actionsCacheCfg()
	st := &state.Store{RegistryCache: &state.RegistryCache{VMID: 101, IP: "192.0.2.224"}}
	in := config.PoolSpec{OS: "linux", ActionsCache: true}

	got := actionsCache(cfg, st, in, "linux")
	if got.Registry != "http://192.0.2.224:5001" || got.Username != "ephemerd" || got.Password != "s3cret" {
		t.Fatalf("opted-in linux pool = %+v", got)
	}
	for name, tc := range map[string]struct {
		spec config.PoolSpec
		os   string
		st   *state.Store
	}{
		"pool not opted in": {config.PoolSpec{OS: "linux"}, "linux", st},
		"windows node":      {config.PoolSpec{OS: "windows", ActionsCache: true}, "windows", st},
		"no cache VM yet":   {in, "linux", &state.Store{}},
		"no address yet":    {in, "linux", &state.Store{RegistryCache: &state.RegistryCache{VMID: 101}}},
	} {
		if got := actionsCache(cfg, tc.st, tc.spec, tc.os); got.Enabled() {
			t.Errorf("%s: rendered %+v, want nothing", name, got)
		}
	}
}

// Opting a pool in must change its members' config hash, so apply pushes the
// new config.toml (and restarts ephemerd on it).
func TestConfigHash_ActionsCacheOptInIsDrift(t *testing.T) {
	cfg := actionsCacheCfg()
	cfg.GitHub = config.GitHub{Owner: "o", AppID: 1, InstallationID: 2, PrivateKeyPath: "/k"}
	st := &state.Store{RegistryCache: &state.RegistryCache{VMID: 101, IP: "192.0.2.224"}}
	pool := &state.Pool{Name: "p"}
	off := config.PoolSpec{Hostname: "ci.example.com", OS: "linux"}
	on := off
	on.ActionsCache = true
	if ConfigHash(cfg, st, off, pool, nil) == ConfigHash(cfg, st, on, pool, nil) {
		t.Fatal("opting a pool into actions_cache did not change its config hash")
	}
	if !strings.Contains(renderForHash(cfg, st, on, pool), "[actions_cache]") {
		t.Fatal("rendered config has no [actions_cache] block")
	}
}

// instanceProvider is fakeProvider whose GetInstance hands back one instance,
// so updateRegistryCacheVM can be driven end to end.
type instanceProvider struct {
	fakeProvider
	inst provider.Instance
}

func (p instanceProvider) GetInstance(context.Context, string, int) (provider.Instance, error) {
	return p.inst, nil
}

// moveUpdateEngine sets up an engine whose cache VM (on "local", 32G) is told
// to move to local-lvm and grow to 200G, against inst.
func moveUpdateEngine(t *testing.T, inst *fakeInstance) (*Engine, *RegistryCacheUpdate) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("USERPROFILE", t.TempDir())
	cfg := dedicatedCacheCfg()
	cfg.RegistryCache.VM.VMStorage = "local-lvm"
	cfg.RegistryCache.VM.DiskGB = 200
	e := &Engine{
		Cfg:       cfg,
		Out:       &bytes.Buffer{},
		provCache: map[config.Provider]provider.Provider{config.ProviderProxmox: instanceProvider{inst: inst}},
		St: &state.Store{RegistryCache: &state.RegistryCache{
			VMID: 101, PVENode: "node-b", IP: "192.0.2.224", DiskGB: 32,
		}},
	}
	u := &RegistryCacheUpdate{
		Cache: cfg.RegistryCache, VMID: 101,
		MoveDiskTo: "local-lvm", DiskGrowGB: 200, FromDiskGB: 32,
	}
	return e, u
}

// The reviewer's blocker: a FAILED move must not be recorded, and nothing may
// be grown — growing on the old storage is how node-b' host root fills up.
func TestUpdateRegistryCacheVM_FailedMoveGrowsNothing(t *testing.T) {
	for name, moveErr := range map[string]error{
		"move task failed":   errors.New("move disk to local-lvm: storage 'local-lvm' does not exist (task UPID:node-b:1)"),
		"move still running": fmt.Errorf("move disk to local-lvm: %w (task UPID:node-b:2)", provider.ErrTaskStillRunning),
	} {
		t.Run(name, func(t *testing.T) {
			inst := &fakeInstance{diskStorage: "local", moveErr: moveErr}
			e, u := moveUpdateEngine(t, inst)
			err := e.updateRegistryCacheVM(context.Background(), u)
			if err == nil {
				t.Fatal("a failed move returned nil")
			}
			if got := e.St.RegistryCache.VMStorage; got != "" {
				t.Fatalf("state recorded VMStorage %q after a failed move", got)
			}
			if got := e.St.RegistryCache.DiskGB; got != 32 {
				t.Fatalf("state recorded disk %dG after a failed move", got)
			}
			if indexOf(inst.calls, "ResizeDisk") >= 0 || indexOf(inst.calls, "Exec") >= 0 {
				t.Fatalf("grew the disk after a failed move: calls %v", inst.calls)
			}
			if errors.Is(moveErr, provider.ErrTaskStillRunning) && !strings.Contains(err.Error(), "re-run apply") {
				t.Errorf("still-running move gives no recovery instruction: %v", err)
			}
		})
	}
}

// Success: move first, then resize, then the in-guest filesystem grow, and
// both are recorded.
func TestUpdateRegistryCacheVM_MoveThenGrow(t *testing.T) {
	inst := &fakeInstance{diskStorage: "local"}
	e, u := moveUpdateEngine(t, inst)
	if err := e.updateRegistryCacheVM(context.Background(), u); err != nil {
		t.Fatal(err)
	}
	mv, rs, gx := indexOf(inst.calls, "MoveDisk"), indexOf(inst.calls, "ResizeDisk"), indexOf(inst.calls, "Exec")
	if mv < 0 || rs < 0 || gx < 0 || !(mv < rs && rs < gx) {
		t.Fatalf("calls %v, want MoveDisk then ResizeDisk then the grow Exec", inst.calls)
	}
	if rc := e.St.RegistryCache; rc.VMStorage != "local-lvm" || rc.DiskGB != 200 {
		t.Fatalf("state = storage %q disk %dG, want local-lvm 200G", rc.VMStorage, rc.DiskGB)
	}
}

// Recovery after a move that outlived an earlier apply's wait: the disk is
// already on the target, so record it and grow — no second move.
func TestUpdateRegistryCacheVM_MoveAlreadyDoneIsRecorded(t *testing.T) {
	inst := &fakeInstance{diskStorage: "local-lvm"}
	e, u := moveUpdateEngine(t, inst)
	if err := e.updateRegistryCacheVM(context.Background(), u); err != nil {
		t.Fatal(err)
	}
	if indexOf(inst.calls, "MoveDisk") >= 0 {
		t.Fatalf("re-moved a disk already on the target: %v", inst.calls)
	}
	if e.St.RegistryCache.VMStorage != "local-lvm" || indexOf(inst.calls, "ResizeDisk") < 0 {
		t.Fatalf("finished move not recorded/grown: storage %q calls %v", e.St.RegistryCache.VMStorage, inst.calls)
	}
}

// A VM config lock means an operation (most likely the earlier move) is still
// in progress: refuse with instructions, don't stack a second move on it.
func TestUpdateRegistryCacheVM_LockedVMRefusesMove(t *testing.T) {
	inst := &fakeInstance{diskStorage: "local", diskLock: "disk"}
	e, u := moveUpdateEngine(t, inst)
	err := e.updateRegistryCacheVM(context.Background(), u)
	if err == nil || !strings.Contains(err.Error(), "locked") {
		t.Fatalf("err = %v, want a locked-VM error", err)
	}
	if indexOf(inst.calls, "MoveDisk") >= 0 || indexOf(inst.calls, "ResizeDisk") >= 0 {
		t.Fatalf("acted on a locked VM: %v", inst.calls)
	}
}

// A failed resize must surface too (ResizeDisk had the same unchecked wait).
func TestUpdateRegistryCacheVM_FailedResizeSurfaces(t *testing.T) {
	inst := &fakeInstance{diskStorage: "local-lvm", resizeErr: errors.New("resize disk: no space (task UPID:node-b:3)")}
	e, u := moveUpdateEngine(t, inst)
	if err := e.updateRegistryCacheVM(context.Background(), u); err == nil {
		t.Fatal("a failed resize returned nil")
	}
	if e.St.RegistryCache.DiskGB != 32 || indexOf(inst.calls, "Exec") >= 0 {
		t.Fatalf("recorded/grew after a failed resize: disk %dG calls %v", e.St.RegistryCache.DiskGB, inst.calls)
	}
}
