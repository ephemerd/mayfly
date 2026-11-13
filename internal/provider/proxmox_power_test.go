package provider

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ephemerd/mayfly/internal/config"
	"github.com/ephemerd/mayfly/internal/pve"
)

// powerPVE is a stateful Proxmox stand-in for power transitions. Unlike
// fakePVE, its tasks can FAIL (Proxmox reports a non-OK exitstatus on a task
// that has stopped running), and the VM's reported status changes on its own
// schedule, which is what the 2026-10-10 reboot failure needed: a qmshutdown
// that failed with "got timeout", and a qmstart answered "already running" by
// a VM that was still powering down.
type powerPVE struct {
	mu     sync.Mutex
	status string // "running" | "stopped"

	// shutdownExit is the exitstatus of a qmshutdown task. "OK" stops the VM.
	shutdownExit string
	// stopsOnOwnAfter, when >0, makes a running VM report stopped after that
	// many further status reads once a (failed) shutdown has been requested:
	// the guest finishing its power-down late, on its own.
	stopsOnOwnAfter int
	shutdownAsked   bool
	readsSinceAsk   int

	starts, stops, shutdowns int
	exits                    map[string]string // task type -> exitstatus
}

func (f *powerPVE) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	path := r.URL.Path
	writeData := func(v interface{}) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"data": v})
	}
	taskUPID := func(kind string) string {
		return "UPID:pve:0000ABCD:00000000:60000000:" + kind + ":100:root@pam:"
	}
	switch {
	case strings.HasSuffix(path, "/status/current"):
		if f.shutdownAsked && f.status == "running" && f.stopsOnOwnAfter > 0 {
			f.readsSinceAsk++
			if f.readsSinceAsk >= f.stopsOnOwnAfter {
				f.status = "stopped"
			}
		}
		writeData(map[string]interface{}{"status": f.status, "vmid": 100, "name": "mfl-test-100"})
	case strings.HasSuffix(path, "/status/shutdown"):
		f.shutdowns++
		f.shutdownAsked = true
		f.exits["qmshutdown"] = f.shutdownExit
		if f.shutdownExit == "OK" {
			f.status = "stopped"
		}
		writeData(taskUPID("qmshutdown"))
	case strings.HasSuffix(path, "/status/stop"):
		f.stops++
		f.exits["qmstop"] = "OK"
		f.status = "stopped"
		writeData(taskUPID("qmstop"))
	case strings.HasSuffix(path, "/status/start"):
		f.starts++
		if f.status == "running" {
			f.exits["qmstart"] = "VM 100 already running"
		} else {
			f.exits["qmstart"] = "OK"
			f.status = "running"
			f.shutdownAsked = false
		}
		writeData(taskUPID("qmstart"))
	case strings.Contains(path, "/tasks/") && strings.HasSuffix(path, "/status"):
		kind := "qmshutdown"
		for _, k := range []string{"qmstart", "qmstop", "qmshutdown"} {
			if strings.Contains(path, ":"+k+":") {
				kind = k
			}
		}
		writeData(map[string]interface{}{"status": "stopped", "exitstatus": f.exits[kind], "upid": taskUPID(kind)})
	default:
		writeData(map[string]interface{}{})
	}
}

func (f *powerPVE) snapshot() (status string, starts, stops, shutdowns int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.status, f.starts, f.stops, f.shutdowns
}

// fastPower compresses the power-state budgets for a test.
func fastPower(t *testing.T) {
	t.Helper()
	oldPoll, oldGrace, oldBudget, oldSettle := powerPollInterval, shutdownFailedGrace, shutdownGraceBudget, startSettle
	powerPollInterval = time.Millisecond
	shutdownFailedGrace = 30 * time.Millisecond
	shutdownGraceBudget = 30 * time.Millisecond
	startSettle = 30 * time.Millisecond
	t.Cleanup(func() {
		powerPollInterval, shutdownFailedGrace, shutdownGraceBudget, startSettle = oldPoll, oldGrace, oldBudget, oldSettle
	})
}

func newPowerInstance(t *testing.T, f *powerPVE) Instance {
	t.Helper()
	if f.exits == nil {
		f.exits = map[string]string{}
	}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	prov := NewProxmox(pve.New(config.Proxmox{URL: srv.URL, TokenID: "test@pam!mayfly", Secret: "s", Node: "pve"}))
	inst, err := prov.GetInstance(context.Background(), "pve", 100)
	if err != nil {
		t.Fatalf("GetInstance: %v", err)
	}
	return inst
}

// The 2026-10-10 failure, shutdown half: with the guest agent dead, qmshutdown
// fails ("VM quit/powerdown failed - got timeout") and the VM keeps running.
// Shutdown used to return nil because the TASK had finished; it must escalate
// to a hard stop and return only once the VM reports stopped.
func TestShutdownEscalatesWhenGracefulTaskFails(t *testing.T) {
	fastPower(t)
	f := &powerPVE{status: "running", shutdownExit: "VM quit/powerdown failed - got timeout"}
	inst := newPowerInstance(t, f)

	if err := inst.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	status, _, stops, shutdowns := f.snapshot()
	if shutdowns != 1 {
		t.Errorf("graceful shutdowns = %d, want 1", shutdowns)
	}
	if stops != 1 {
		t.Errorf("hard stops = %d, want 1 (escalation after the failed graceful shutdown)", stops)
	}
	if status != "stopped" {
		t.Errorf("VM status = %q after Shutdown, want stopped", status)
	}
}

// A failed shutdown task whose VM does finish going down on its own inside the
// grace window needs no hard stop.
func TestShutdownWaitsOutLateStopWithoutHardStop(t *testing.T) {
	fastPower(t)
	f := &powerPVE{status: "running", shutdownExit: "VM quit/powerdown failed - got timeout", stopsOnOwnAfter: 3}
	inst := newPowerInstance(t, f)

	if err := inst.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	status, _, stops, _ := f.snapshot()
	if stops != 0 {
		t.Errorf("hard stops = %d, want 0 (the VM stopped by itself within the grace window)", stops)
	}
	if status != "stopped" {
		t.Errorf("VM status = %q, want stopped", status)
	}
}

// PVE fails a qmshutdown on its own (shorter) timeout while a healthy guest
// is still going down. That guest gets the rest of the graceful budget, not
// just the short post-failure grace, before anything hard-stops it.
func TestShutdownFailedTaskGetsRestOfGracefulBudget(t *testing.T) {
	fastPower(t)
	shutdownFailedGrace = time.Millisecond
	shutdownGraceBudget = 10 * time.Second
	// At >=1ms per poll, 50 reads outlast the 1ms grace by far, but finish
	// well inside the budget.
	f := &powerPVE{status: "running", shutdownExit: "VM quit/powerdown failed - got timeout", stopsOnOwnAfter: 50}
	inst := newPowerInstance(t, f)

	if err := inst.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	status, _, stops, _ := f.snapshot()
	if stops != 0 {
		t.Errorf("hard stops = %d, want 0 (a slow graceful shutdown was cut off before the budget ran out)", stops)
	}
	if status != "stopped" {
		t.Errorf("VM status = %q, want stopped", status)
	}
}

// An already-stopped VM is left alone.
func TestShutdownAlreadyStoppedIsNoop(t *testing.T) {
	fastPower(t)
	f := &powerPVE{status: "stopped", shutdownExit: "OK"}
	inst := newPowerInstance(t, f)
	if err := inst.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if _, _, stops, shutdowns := f.snapshot(); stops != 0 || shutdowns != 0 {
		t.Errorf("stopped VM was sent shutdown=%d stop=%d, want none", shutdowns, stops)
	}
}

// The 2026-10-10 failure, start half: qmstart against a VM that is still
// powering down fails "already running", and the VM then finishes stopping.
// Start used to return nil on the finished task; it must notice the VM drop to
// stopped and start it again, returning only once it stays running.
func TestStartRetriesWhenAlreadyRunningVMThenStops(t *testing.T) {
	fastPower(t)
	f := &powerPVE{status: "running", shutdownAsked: true, stopsOnOwnAfter: 3, exits: map[string]string{}}
	inst := newPowerInstance(t, f)

	if err := inst.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	status, starts, _, _ := f.snapshot()
	if starts != 2 {
		t.Errorf("start requests = %d, want 2 (refused while going down, then re-issued once stopped)", starts)
	}
	if status != "running" {
		t.Errorf("VM status = %q after Start, want running", status)
	}
}

// A plain start of a stopped VM issues one start and confirms running.
func TestStartStoppedVM(t *testing.T) {
	fastPower(t)
	f := &powerPVE{status: "stopped"}
	inst := newPowerInstance(t, f)
	if err := inst.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if status, starts, _, _ := f.snapshot(); starts != 1 || status != "running" {
		t.Errorf("starts=%d status=%q, want 1 start and running", starts, status)
	}
}

// IsRunning must read live status, not the handle's cached copy from when the
// instance was looked up.
func TestIsRunningReadsLiveStatus(t *testing.T) {
	fastPower(t)
	f := &powerPVE{status: "running"}
	inst := newPowerInstance(t, f)
	f.mu.Lock()
	f.status = "stopped"
	f.mu.Unlock()
	running, err := inst.IsRunning(context.Background())
	if err != nil {
		t.Fatalf("IsRunning: %v", err)
	}
	if running {
		t.Error("IsRunning = true for a VM that has since stopped (stale cached status)")
	}
}
