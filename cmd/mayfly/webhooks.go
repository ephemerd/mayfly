package main

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/urfave/cli/v3"

	"github.com/ephemerd/mayfly/internal/config"
	"github.com/ephemerd/mayfly/internal/ghapp"
	"github.com/ephemerd/mayfly/internal/github"
	"github.com/ephemerd/mayfly/internal/state"
)

func webhooksCmd() *cli.Command {
	return &cli.Command{
		Name:  "webhooks",
		Usage: "Inspect and reconcile fleet webhooks across the org",
		Commands: []*cli.Command{
			{
				Name:   "list",
				Usage:  "Scan the org and classify every workflow_job webhook",
				Action: func(ctx context.Context, c *cli.Command) error { return runWebhooks(ctx, false, false, false) },
			},
			{
				Name:  "prune",
				Usage: "Delete orphaned fleet webhooks (base-domain hosts not backed by a live pool)",
				Flags: []cli.Flag{
					&cli.BoolFlag{Name: "auto-approve", Usage: "skip the confirmation prompt"},
					&cli.BoolFlag{Name: "dead-only", Usage: "only delete orphans whose last delivery failed (leave still-serving old nodes alone)"},
				},
				Action: func(ctx context.Context, c *cli.Command) error {
					return runWebhooks(ctx, true, c.Bool("auto-approve"), c.Bool("dead-only"))
				},
			},
		},
	}
}

type hookRef struct {
	repo  github.Repo
	hook  github.Hook
	class github.HookClass
}

func runWebhooks(ctx context.Context, prune, autoApprove, deadOnly bool) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	creds, err := ghapp.Load(cfg.GitHub)
	if err != nil {
		return err
	}
	st, err := state.Load()
	if err != nil {
		return err
	}

	live := map[string]bool{}
	for _, p := range st.Pools {
		if p.Hostname != "" {
			live[p.Hostname] = true
		}
	}

	token, err := creds.InstallationToken(ctx)
	if err != nil {
		return err
	}
	gh := github.New(token)

	repos, err := gh.ListOrgRepos(ctx, cfg.GitHub.Owner)
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stdout, "scanning %d repos in %s (base domain %s)\n\n", len(repos), cfg.GitHub.Owner, cfg.Cloudflare.BaseDomain)

	var orphans []hookRef
	var liveCount, foreignCount int
	sort.Slice(repos, func(i, j int) bool { return repos[i].FullName < repos[j].FullName })
	for _, r := range repos {
		hooks, err := gh.ListRepoHooks(ctx, r.Owner.Login, r.Name)
		if err != nil {
			fmt.Fprintf(os.Stderr, "  ! %s: %v\n", r.FullName, err)
			continue
		}
		for _, h := range hooks {
			class := github.Classify(h, cfg.Cloudflare.BaseDomain, live)
			switch class {
			case github.Live:
				liveCount++
				fmt.Fprintf(os.Stdout, "  keep   %-28s %s\n", r.FullName, h.Config.URL)
			case github.Orphan:
				orphans = append(orphans, hookRef{r, h, class})
				fmt.Fprintf(os.Stdout, "  ORPHAN %-28s %s (last=%s)\n", r.FullName, h.Config.URL, statusOf(h))
			case github.Foreign:
				foreignCount++
			}
		}
	}

	fmt.Fprintf(os.Stdout, "\n%d live, %d orphan, %d foreign (left alone)\n", liveCount, len(orphans), foreignCount)

	targets := orphans
	if deadOnly {
		targets = targets[:0]
		for _, o := range orphans {
			if !isDelivering(o.hook) {
				targets = append(targets, o)
			}
		}
		fmt.Fprintf(os.Stdout, "%d of those are dead (--dead-only)\n", len(targets))
	}

	if len(targets) == 0 || !prune {
		if len(orphans) > 0 && !prune {
			fmt.Fprintln(os.Stdout, "run `mayfly webhooks prune` (add --dead-only to spare still-serving old nodes)")
		}
		return nil
	}

	if !autoApprove && !confirmPrune(len(targets)) {
		fmt.Fprintln(os.Stdout, "aborted")
		return nil
	}
	for _, o := range targets {
		if err := gh.DeleteRepoHook(ctx, o.repo.Owner.Login, o.repo.Name, o.hook.ID); err != nil {
			fmt.Fprintf(os.Stderr, "  ! delete %s hook %d: %v\n", o.repo.FullName, o.hook.ID, err)
			continue
		}
		fmt.Fprintf(os.Stdout, "  deleted %s %s\n", o.repo.FullName, o.hook.Config.URL)
	}
	return nil
}

func statusOf(h github.Hook) string {
	if h.LastResponse.Status == "" {
		return "unknown"
	}
	return h.LastResponse.Status
}

// isDelivering reports whether GitHub's last delivery to the hook succeeded.
// A never-pinged or failed hook (missing / connection_error / non-2xx) counts
// as dead. Only "active" / "ok" are treated as still-serving.
func isDelivering(h github.Hook) bool {
	switch h.LastResponse.Status {
	case "active", "ok":
		return true
	default:
		return false
	}
}

func confirmPrune(n int) bool {
	fmt.Printf("\nDelete %d orphaned webhook(s)? [y/N]: ", n)
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil {
		return false
	}
	a := strings.ToLower(strings.TrimSpace(line))
	return a == "y" || a == "yes"
}
