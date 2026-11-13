package main

import (
	"context"
	"fmt"
	"os"

	"github.com/urfave/cli/v3"
)

func modelServerCmd() *cli.Command {
	return &cli.Command{
		Name:  "model-server",
		Usage: "Provision or reconcile just the GPU model-server VM",
		Commands: []*cli.Command{
			modelServerApplyCmd(),
		},
	}
}

func modelServerApplyCmd() *cli.Command {
	return &cli.Command{
		Name: "apply",
		Usage: "Reconcile the [model_server] VM only, skipping the full-fleet plan " +
			"(use when a down node blocks `mayfly apply`)",
		Flags: []cli.Flag{
			&cli.BoolFlag{Name: "auto-approve", Usage: "skip the confirmation prompt"},
		},
		Action: runModelServerApply,
	}
}

func runModelServerApply(ctx context.Context, c *cli.Command) error {
	e, err := newEngine()
	if err != nil {
		return err
	}
	p := e.PlanModelServer()
	p.Render(os.Stdout)
	if p.Empty() {
		return nil
	}
	if !c.Bool("auto-approve") && !confirm("Apply this model-server change?") {
		fmt.Println("aborted")
		return nil
	}
	if err := e.ApplyModelServer(ctx, p); err != nil {
		return fmt.Errorf("model-server apply failed (state is saved up to the failing step): %w", err)
	}
	return nil
}
