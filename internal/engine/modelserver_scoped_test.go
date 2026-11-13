package engine

import (
	"testing"

	"github.com/ephemerd/mayfly/internal/state"
)

// PlanModelServer must reach the same verdict as a full plan's
// diffModelServerVM, but touch nothing else: an engine whose config has a dozen
// pools still produces a plan with only the ModelServer* fields set.
func TestPlanModelServer_OnlyTouchesTheServer(t *testing.T) {
	cfg := modelServerCfg()
	e := &Engine{Cfg: cfg, St: &state.Store{}}

	p := e.PlanModelServer()
	if p.ModelServerCreate == nil {
		t.Fatal("a server absent from state did not plan a create")
	}
	if len(p.PoolCreates) != 0 || len(p.Updates) != 0 || len(p.TemplateCreates) != 0 ||
		p.MonitoringCreate != nil || p.MonitoringUpdate != nil || p.RegistryCacheCreate != nil {
		t.Fatalf("scoped plan leaked non-model-server work: %+v", p)
	}
}

func TestPlanModelServer_InSyncIsEmpty(t *testing.T) {
	cfg := modelServerCfg()
	e := &Engine{Cfg: cfg, St: &state.Store{ModelServer: recorded(cfg)}}
	if p := e.PlanModelServer(); !p.Empty() {
		t.Fatalf("an in-sync server planned work: %+v", p)
	}
}
