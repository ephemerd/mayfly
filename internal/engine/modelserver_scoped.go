package engine

import (
	"context"
	"fmt"
)

// PlanModelServer computes the model-server VM's drift in isolation, with no
// live refresh and without touching any other fleet resource. It is the scoped
// counterpart to BuildPlan for operators who need to bring the GPU server up or
// reconcile it while the rest of the fleet cannot be planned — e.g. a runner
// node is down, which makes a full `mayfly apply` abort before it ever reaches
// the model server (the apply applies template creates and pool updates first).
//
// The returned plan has only the ModelServer* fields set; everything else is
// zero, so Render and ApplyModelServer see just this one resource.
func (e *Engine) PlanModelServer() *Plan {
	p := &Plan{}
	diffModelServerVM(e.Cfg, e.St, p)
	return p
}

// ApplyModelServer applies only the model-server portion of p, in the same
// order the full apply uses (destroy releases the GPU before a create claims
// it). It deliberately does NOT run the template/pool/monitoring/registry
// steps, nor foldTargetDrift: the model server is not a scrape target, and the
// whole point of the scoped command is to leave the rest of the fleet alone.
func (e *Engine) ApplyModelServer(ctx context.Context, p *Plan) error {
	if p.ModelServerDestroy {
		if err := e.destroyModelServerVM(ctx); err != nil {
			return fmt.Errorf("destroy model server vm: %w", err)
		}
	}
	if p.ModelServerCreate != nil {
		if err := e.provisionModelServerVM(ctx, p.ModelServerCreate.Server); err != nil {
			return fmt.Errorf("provision model server vm: %w", err)
		}
	}
	if p.ModelServerUpdate != nil {
		if err := e.updateModelServerVM(ctx, p.ModelServerUpdate); err != nil {
			return fmt.Errorf("update model server vm: %w", err)
		}
	}
	return nil
}
