package provider

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ephemerd/mayfly/internal/config"
	"github.com/ephemerd/mayfly/internal/pve"
)

// diskPVE answers the disk calls: a move_disk or resize returns a task whose
// final exitstatus is set per test, the way Proxmox reports a task that has
// stopped running but FAILED.
type diskPVE struct {
	exit  string // exitstatus of the move/resize task
	scsi0 string
	lock  string
}

func (f *diskPVE) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	writeData := func(v interface{}) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"data": v})
	}
	upid := func(kind string) string {
		return "UPID:pve:0000ABCD:00000000:60000000:" + kind + ":100:root@pam:"
	}
	switch {
	case strings.HasSuffix(path, "/status/current"):
		writeData(map[string]interface{}{"status": "running", "vmid": 100, "name": "mfl-registry-cache-100"})
	case strings.HasSuffix(path, "/config"):
		cfg := map[string]interface{}{"scsi0": f.scsi0}
		if f.lock != "" {
			cfg["lock"] = f.lock
		}
		writeData(cfg)
	case strings.HasSuffix(path, "/move_disk"):
		writeData(upid("qmmove"))
	case strings.HasSuffix(path, "/resize"):
		writeData(upid("resize"))
	case strings.Contains(path, "/tasks/") && strings.HasSuffix(path, "/status"):
		writeData(map[string]interface{}{"status": "stopped", "exitstatus": f.exit})
	default:
		writeData(map[string]interface{}{})
	}
}

func newDiskInstance(t *testing.T, f *diskPVE) Instance {
	t.Helper()
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	prov := NewProxmox(pve.New(config.Proxmox{URL: srv.URL, TokenID: "test@pam!mayfly", Secret: "s", Node: "pve"}))
	inst, err := prov.GetInstance(context.Background(), "pve", 100)
	if err != nil {
		t.Fatalf("GetInstance: %v", err)
	}
	return inst
}

// go-proxmox's Task.Wait returns nil for a task that stopped with a failure.
// MoveDisk and ResizeDisk must read the exit status and fail, or a failed move
// is recorded as done and the disk grown on the old storage.
func TestDiskTasksFailOnNonOKExit(t *testing.T) {
	f := &diskPVE{exit: "storage 'local-lvm' does not exist", scsi0: "local:100/vm-100-disk-0.qcow2,size=32G"}
	inst := newDiskInstance(t, f)
	if err := inst.MoveDisk(context.Background(), "local-lvm"); err == nil || !strings.Contains(err.Error(), "does not exist") {
		t.Fatalf("MoveDisk on a failed task = %v, want the task's failure", err)
	}
	if err := inst.ResizeDisk(context.Background(), 200); err == nil {
		t.Fatal("ResizeDisk on a failed task returned nil")
	}

	f.exit = "OK"
	if err := inst.MoveDisk(context.Background(), "local-lvm"); err != nil {
		t.Fatalf("MoveDisk on an OK task = %v", err)
	}
	if err := inst.ResizeDisk(context.Background(), 200); err != nil {
		t.Fatalf("ResizeDisk on an OK task = %v", err)
	}
}

func TestDiskStorageReadsConfig(t *testing.T) {
	inst := newDiskInstance(t, &diskPVE{scsi0: "local-lvm:vm-100-disk-0,size=200G", lock: "disk"})
	storage, lock, err := inst.DiskStorage(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if storage != "local-lvm" || lock != "disk" {
		t.Fatalf("DiskStorage = %q, %q; want local-lvm, disk", storage, lock)
	}
}
