package main

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/urfave/cli/v3"

	"github.com/luthermonson/mayfly/internal/config"
	"github.com/luthermonson/mayfly/internal/engine"
	"github.com/luthermonson/mayfly/internal/state"
)

func planCmd() *cli.Command {
	return &cli.Command{
		Name:  "plan",
		Usage: "Show what apply would change, without changing anything",
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "replace", Usage: "force replacement of all members in the named pool"},
			&cli.BoolFlag{Name: "no-refresh", Usage: "skip reconciling recorded state against live infrastructure"},
			&cli.BoolFlag{Name: "ssh-probe", Value: true, Usage: "probe darwin members over SSH during refresh (15s per host)"},
		},
		Action: runPlan,
	}
}

func applyCmd() *cli.Command {
	return &cli.Command{
		Name:  "apply",
		Usage: "Reconcile the fleet to match [pools.*] in config.toml",
		Flags: []cli.Flag{
			&cli.BoolFlag{Name: "auto-approve", Usage: "skip the confirmation prompt"},
			&cli.StringFlag{Name: "replace", Usage: "force replacement of all members in the named pool (wedged-node escape hatch)"},
			&cli.BoolFlag{Name: "no-refresh", Usage: "skip reconciling recorded state against live infrastructure"},
			&cli.BoolFlag{Name: "ssh-probe", Value: true, Usage: "probe darwin members over SSH during refresh (15s per host)"},
		},
		Action: runApply,
	}
}

func destroyCmd() *cli.Command {
	return &cli.Command{
		Name:      "destroy",
		Usage:     "Destroy a pool (or the entire fleet when no pool is named)",
		ArgsUsage: "[pool]",
		Flags: []cli.Flag{
			&cli.BoolFlag{Name: "auto-approve", Usage: "skip the confirmation prompt"},
		},
		Action: runDestroy,
	}
}

func newEngine() (*engine.Engine, error) {
	cfg, err := config.Load()
	if err != nil {
		return nil, err
	}
	st, err := state.Load()
	if err != nil {
		return nil, err
	}
	return engine.New(cfg, st, os.Stdout)
}

func runPlan(ctx context.Context, c *cli.Command) error {
	e, err := newEngine()
	if err != nil {
		return err
	}
	p, rr, err := e.BuildPlan(ctx, c.String("replace"), !c.Bool("no-refresh"), c.Bool("ssh-probe"))
	if err != nil {
		return err
	}
	rr.Render(os.Stdout)
	p.Render(os.Stdout)
	return nil
}

func runApply(ctx context.Context, c *cli.Command) error {
	e, err := newEngine()
	if err != nil {
		return err
	}
	p, rr, err := e.BuildPlan(ctx, c.String("replace"), !c.Bool("no-refresh"), c.Bool("ssh-probe"))
	if err != nil {
		return err
	}
	rr.Render(os.Stdout)
	p.Render(os.Stdout)
	if p.Empty() {
		// Nothing to change on the fleet, but the refresh may still have
		// corrected bookkeeping (a node's live ephemerd version, a migrated
		// config fingerprint). Keep it, so the next plan starts from it.
		return e.St.Save()
	}
	if !c.Bool("auto-approve") && !confirm("Apply these changes?") {
		fmt.Println("aborted")
		return nil
	}
	if err := e.Apply(ctx, p); err != nil {
		return fmt.Errorf("apply failed (state is saved up to the failing step; re-run apply to resume): %w", err)
	}
	return nil
}

func runDestroy(ctx context.Context, c *cli.Command) error {
	e, err := newEngine()
	if err != nil {
		return err
	}
	p, err := e.DestroyPlan(c.Args().First())
	if err != nil {
		return err
	}
	p.Render(os.Stdout)
	if p.Empty() {
		return nil
	}
	if !c.Bool("auto-approve") && !confirm("Destroy these resources?") {
		fmt.Println("aborted")
		return nil
	}
	return e.Apply(ctx, p)
}

func confirm(prompt string) bool {
	fmt.Printf("\n%s [y/N]: ", prompt)
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil {
		return false
	}
	answer := strings.ToLower(strings.TrimSpace(line))
	return answer == "y" || answer == "yes"
}
