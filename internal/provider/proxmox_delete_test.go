package provider

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/luthermonson/mayfly/internal/config"
	"github.com/luthermonson/mayfly/internal/pve"
)

// fakePVE is a minimal Proxmox API stand-in that records the ordered sequence
// of (method + path) calls, so a test can assert what Delete did against a
// running vs. a stopped VM without a live hypervisor.
type fakePVE struct {
	mu     sync.Mutex
	calls  []string
	status string // VM power state reported by status/current: "running" | "stopped"
}

// upid is a well-formed task id whose node segment ("pve") matches the test
// node, so go-proxmox's Task.Wait polls /nodes/pve/tasks/<upid>/status.
const upid = "UPID:pve:0000ABCD:00000000:60000000:qmshutdown:100:root@pam:"

func (f *fakePVE) record(method, path string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, method+" "+path)
}

func (f *fakePVE) sequence() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.calls))
	copy(out, f.calls)
	return out
}

func (f *fakePVE) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	f.record(r.Method, path)
	writeData := func(v interface{}) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"data": v})
	}
	switch {
	case strings.HasSuffix(path, "/status/current"):
		f.mu.Lock()
		st := f.status
		f.mu.Unlock()
		writeData(map[string]interface{}{"status": st, "vmid": 100, "name": "mfl-test-100"})
	case strings.HasSuffix(path, "/config"):
		writeData(map[string]interface{}{})
	case strings.HasSuffix(path, "/nodes/pve/status"):
		writeData(map[string]interface{}{})
	case strings.HasSuffix(path, "/status/shutdown"), strings.HasSuffix(path, "/status/stop"):
		// The power-off lands: Shutdown/Stop now wait for the VM to REPORT
		// stopped rather than trusting the task.
		f.mu.Lock()
		f.status = "stopped"
		f.mu.Unlock()
		writeData(upid)
	case strings.Contains(path, "/tasks/") && strings.HasSuffix(path, "/status"):
		// Report the task as already finished successfully.
		writeData(map[string]interface{}{"status": "stopped", "exitstatus": "OK", "upid": upid})
	case r.Method == http.MethodDelete:
		writeData(upid)
	default:
		writeData(map[string]interface{}{})
	}
}

func newFakeProvider(t *testing.T, status string) (*Proxmox, *fakePVE) {
	t.Helper()
	fake := &fakePVE{status: status}
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)
	pv := pve.New(config.Proxmox{
		URL:     srv.URL,
		TokenID: "test@pam!mayfly",
		Secret:  "secret",
		Node:    "pve",
	})
	return NewProxmox(pv), fake
}

// TestDelete_StopsRunningVMBeforeDestroy is the regression for the rolling-
// replace bug: destroying a running VM without powering it off first makes
// Proxmox answer "VM <id> is running - destroy failed", aborting the apply with
// the old and new members both alive. Delete must stop the VM before it issues
// the DELETE.
func TestDelete_StopsRunningVMBeforeDestroy(t *testing.T) {
	prov, fake := newFakeProvider(t, "running")

	inst, err := prov.GetInstance(context.Background(), "pve", 100)
	if err != nil {
		t.Fatalf("GetInstance: %v", err)
	}
	if err := inst.Delete(context.Background()); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	seq := fake.sequence()
	shutdownAt, deleteAt := -1, -1
	for i, c := range seq {
		if strings.Contains(c, "/status/shutdown") || strings.Contains(c, "/status/stop") {
			if shutdownAt == -1 {
				shutdownAt = i
			}
		}
		if strings.HasPrefix(c, "DELETE ") && strings.HasSuffix(c, "/qemu/100") {
			deleteAt = i
		}
	}
	if shutdownAt == -1 {
		t.Fatalf("Delete never powered the running VM off; calls: %v", seq)
	}
	if deleteAt == -1 {
		t.Fatalf("Delete never issued the DELETE; calls: %v", seq)
	}
	if shutdownAt > deleteAt {
		t.Fatalf("VM was destroyed before being stopped (shutdown at %d, delete at %d); calls: %v",
			shutdownAt, deleteAt, seq)
	}
}

// TestDelete_SkipsStopForStoppedVM keeps the change idempotent: a VM that is
// already stopped must not be sent a shutdown/stop (Proxmox errors "VM is not
// running"), only the DELETE.
func TestDelete_SkipsStopForStoppedVM(t *testing.T) {
	prov, fake := newFakeProvider(t, "stopped")

	inst, err := prov.GetInstance(context.Background(), "pve", 100)
	if err != nil {
		t.Fatalf("GetInstance: %v", err)
	}
	if err := inst.Delete(context.Background()); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	seq := fake.sequence()
	deleted := false
	for _, c := range seq {
		if strings.Contains(c, "/status/shutdown") || strings.Contains(c, "/status/stop") {
			t.Fatalf("Delete powered off an already-stopped VM; calls: %v", seq)
		}
		if strings.HasPrefix(c, "DELETE ") && strings.HasSuffix(c, "/qemu/100") {
			deleted = true
		}
	}
	if !deleted {
		t.Fatalf("Delete never issued the DELETE; calls: %v", seq)
	}
}
