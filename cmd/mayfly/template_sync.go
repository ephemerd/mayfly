package main

import (
	"context"
	"fmt"
	"os"

	"github.com/urfave/cli/v3"

	"github.com/luthermonson/mayfly/internal/config"
	"github.com/luthermonson/mayfly/internal/engine"
	"github.com/luthermonson/mayfly/internal/pve"
)

func templateSyncCmd() *cli.Command {
	return &cli.Command{
		Name:   "sync",
		Usage:  "Create the base linux templates referenced by [pools.*] (idempotent; apply also does this)",
		Action: runTemplateSync,
	}
}

func runTemplateSync(ctx context.Context, _ *cli.Command) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	pv := pve.New(cfg.Proxmox)

	seen := map[string]bool{}
	for name, raw := range cfg.Pools {
		spec := raw.Resolved(cfg.Defaults)
		if spec.OS != "linux" {
			continue
		}
		// An enrolled pool provisions no VM and carries an empty Template.
		if spec.UsesEnroll() {
			continue
		}
		node, storage := spec.ResolvedPlacement(cfg.Proxmox)
		key := spec.Template + "@" + node
		if seen[key] {
			continue
		}
		seen[key] = true
		fmt.Fprintf(os.Stdout, "syncing template %s (%s) on %s for pool %s\n", spec.Template, spec.Arch, node, name)
		if err := pv.SyncTemplate(ctx, pve.TemplateSyncOptions{
			Name:     spec.Template,
			VMID:     engine.TemplateVMID(spec.Arch),
			Arch:     spec.Arch,
			Node:     node,
			Storage:  storage,
			ImageURL: engine.DefaultImageURL(spec.Arch),
		}); err != nil {
			return fmt.Errorf("template %s: %w", spec.Template, err)
		}
	}
	if len(seen) == 0 {
		fmt.Fprintln(os.Stdout, "no linux pools configured; nothing to sync")
	}
	return nil
}
