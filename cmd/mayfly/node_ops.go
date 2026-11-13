package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/urfave/cli/v3"

	"github.com/ephemerd/mayfly/internal/config"
	"github.com/ephemerd/mayfly/internal/engine"
	"github.com/ephemerd/mayfly/internal/ghapp"
	"github.com/ephemerd/mayfly/internal/provider"
	"github.com/ephemerd/mayfly/internal/state"
)

// Every subcommand below is transport-agnostic: it names a node, gets a
// engine.NodeExecutor for it, and runs a command. Whether that command travels
// over the Proxmox guest agent or SSH is decided once, in
// engine.ResolveNodeTarget, from the node's pool config — see
// internal/engine/nodeexec.go.

func nodeListCmd() *cli.Command {
	return &cli.Command{
		Name:  "list",
		Usage: "List all pool members",
		Action: func(_ context.Context, _ *cli.Command) error {
			st, err := state.Load()
			if err != nil {
				return err
			}
			if len(st.Pools) == 0 {
				fmt.Println("no pools")
				return nil
			}
			fmt.Printf("%-16s  %-6s  %-26s  %-14s  %-32s  %s\n", "POOL", "VMID", "NAME", "OS/ARCH", "HOSTNAME", "CREATED")
			for _, p := range st.Pools {
				for _, m := range p.Members {
					fmt.Printf("%-16s  %-6d  %-26s  %-14s  %-32s  %s\n",
						p.Name, m.VMID, m.Hostname, m.OS+"/"+m.Arch, p.Hostname, m.CreatedAt.Format("2006-01-02T15:04Z07:00"))
				}
			}
			return nil
		},
	}
}

func nodeStatusCmd() *cli.Command {
	return &cli.Command{
		Name:      "status",
		Usage:     "Show ephemerd status on a node",
		ArgsUsage: "<name>",
		Action: func(ctx context.Context, c *cli.Command) error {
			return runOnNode(ctx, c.Args().First(), 60, func(_ *config.Config, m *state.Member) ([]string, error) {
				return engine.EphemerdArgv(m.OS, "status"), nil
			})
		},
	}
}

// logsExecTimeoutSec bounds a one-shot log tail; logsFollowTimeoutSec bounds a
// --follow, which over the guest agent cannot stream live and so is capped
// rather than left open (see engine.LogsArgv).
const (
	logsExecTimeoutSec   = 60
	logsFollowTimeoutSec = 3600
)

func nodeLogsCmd() *cli.Command {
	return &cli.Command{
		Name:      "logs",
		Usage:     "Tail a node's ephemerd logs",
		ArgsUsage: "<name>",
		Flags: []cli.Flag{
			&cli.BoolFlag{
				Name:    "follow",
				Aliases: []string{"f"},
				Usage:   "keep streaming new entries (live only over SSH; bounded, not live, over the guest agent)",
			},
			&cli.IntFlag{
				Name:    "lines",
				Aliases: []string{"n"},
				Value:   engine.DefaultLogLines,
				Usage:   "number of trailing log entries to show",
			},
		},
		Action: func(ctx context.Context, c *cli.Command) error {
			res, err := resolveNode(c.Args().First())
			if err != nil {
				return err
			}
			follow := c.Bool("follow")
			timeout := logsExecTimeoutSec
			switch {
			case follow && res.member.OS == "windows":
				// Get-WinEvent has no follow; say so rather than pretend.
				fmt.Fprintln(os.Stderr, "note: --follow is not supported on windows; showing the most recent entries instead")
				follow = false
			case follow && !engine.LogsFollowSupported(res.target.Transport):
				fmt.Fprintf(os.Stderr,
					"note: --follow does not stream live over the guest agent; bounded to %ds, then it returns what it buffered\n",
					logsFollowTimeoutSec)
				timeout = logsFollowTimeoutSec
			case follow:
				timeout = logsFollowTimeoutSec
			}
			argv := engine.LogsArgv(res.member.OS, c.Int("lines"), follow)
			return report(res.exec.Exec(ctx, argv, timeout))
		},
	}
}

func nodeDrainCmd() *cli.Command {
	return &cli.Command{
		Name:      "drain",
		Usage:     "Gracefully drain a node",
		ArgsUsage: "<name>",
		Action: func(ctx context.Context, c *cli.Command) error {
			return runOnNode(ctx, c.Args().First(), 600, func(_ *config.Config, m *state.Member) ([]string, error) {
				return engine.EphemerdArgv(m.OS, "drain"), nil
			})
		},
	}
}

// nodeUpgradeCmd upgrades ephemerd on one node. It probes the daemon's version
// first: v0.1.7+ upgrades itself over HTTPS (`ephemerd upgrade --version`),
// which needs no hypervisor channel and so behaves identically on every
// provider; anything older (or a daemon too old to report a version) gets the
// historical binary push. engine.PlanNodeUpgrade owns that decision.
//
// Either way the command ends by asking the NODE what happened rather than
// trusting the exec channel it travelled over — see internal/engine's
// nodeupgradewait.go for why the exec channel is not a witness. Both paths
// need this equally: --push downloads ~1 GB in-guest and bounces the service
// with Stop-Service/Start-Service, which breaks the guest-agent poll exactly
// like the self-upgrade's restart does.
func nodeUpgradeCmd() *cli.Command {
	return &cli.Command{
		Name:      "upgrade",
		Usage:     "Upgrade ephemerd on a node",
		ArgsUsage: "<name>",
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "version", Usage: "ephemerd version to install (defaults to config)"},
			&cli.BoolFlag{Name: "push", Usage: "force the binary-push path instead of the daemon's self-upgrade"},
			&cli.BoolFlag{Name: "dry-run", Usage: "probe the node and print the upgrade it would run, without running it"},
			&cli.DurationFlag{
				Name:  "wait",
				Value: engine.UpgradeVerifyTimeout,
				Usage: "how long to wait for the node to come back on the new version before declaring the upgrade unverified",
			},
			&cli.BoolFlag{Name: "no-wait", Usage: "fire the upgrade and exit without verifying the node came back (outcome unknown)"},
		},
		Action: func(ctx context.Context, c *cli.Command) error {
			res, err := resolveNode(c.Args().First())
			if err != nil {
				return err
			}
			spec, _ := res.spec()
			src := engine.EphemerdSource(res.cfg.Ephemerd, spec)
			if v := c.String("version"); v != "" {
				src.Version = v
				src.BinaryURL = ""
			}

			reported := ""
			if !c.Bool("push") {
				reported = engine.ProbeEphemerdVersion(ctx, res.exec, res.member.OS)
			}
			up, err := engine.PlanNodeUpgrade(src, res.member.OS, res.member.Arch, reported)
			if err != nil {
				return err
			}
			switch up.Method {
			case engine.UpgradeSelf:
				fmt.Fprintf(os.Stderr, "%s (%s): %s → %s (daemon self-upgrade)\n",
					res.member.Hostname, res.target.Transport, describeVersion(reported), up.Version)
			default:
				fmt.Fprintf(os.Stderr, "%s (%s): %s → %s (binary push: %s)\n",
					res.member.Hostname, res.target.Transport, describeVersion(reported), up.Version, up.URL)
			}
			if c.Bool("dry-run") {
				fmt.Fprintf(os.Stderr, "dry run; would execute: %s\n", engine.ShellJoin(up.Argv))
				return nil
			}

			execRes, execErr := res.exec.Exec(ctx, up.Argv, upgradeExecTimeoutSec)
			if execErr == nil {
				writeStream(os.Stdout, execRes.Stdout)
				writeStream(os.Stderr, execRes.Stderr)
			}
			if c.Bool("no-wait") {
				if execErr != nil {
					return execErr
				}
				if execRes.ExitCode != 0 {
					return fmt.Errorf("exit code %d", execRes.ExitCode)
				}
				fmt.Fprintln(os.Stderr, "not waiting; run `mayfly node status` to see whether the upgrade landed")
				return nil
			}

			// The exec's own outcome is a hint, not the verdict. Say what it
			// was — it is genuinely useful context when the node then fails to
			// come back — but do not return on it.
			switch {
			case execErr != nil:
				fmt.Fprintf(os.Stderr, "lost sight of the upgrade command (%v); verifying against the node instead\n", execErr)
			case execRes.ExitCode != 0:
				fmt.Fprintf(os.Stderr, "upgrade command exited %d; verifying against the node anyway\n", execRes.ExitCode)
			}

			wait := c.Duration("wait")
			fmt.Fprintf(os.Stderr, "waiting up to %s for %s to report %s and resume taking jobs...\n",
				wait, res.member.Hostname, describeTarget(up.Version))
			logf := func(format string, args ...any) { fmt.Fprintf(os.Stderr, format+"\n", args...) }
			verdict, err := engine.AwaitUpgrade(ctx, res.exec, res.member.OS, up.Version, wait, logf)
			if err != nil {
				return err
			}
			if !verdict.Done() {
				return engine.DescribeUpgradeFailure(verdict, describeTarget(up.Version), wait, engine.EphemerdExe(res.member.OS))
			}
			fmt.Fprintf(os.Stderr, "%s upgraded: %s\n", res.member.Hostname, verdict.Health)

			// Record the new binary in state. Without this, plan kept judging
			// the node against the binary it was last applied with and offered
			// to restart it. The GitHub App key is only needed to migrate a
			// pre-split config fingerprint; a node command does not otherwise
			// require it, so its absence just skips that migration.
			var pem []byte
			if gh, gerr := ghapp.Load(res.cfg.GitHub); gerr == nil {
				pem = gh.PrivateKeyPEM
			}
			engine.RecordUpgrade(res.cfg, res.st, res.pool, res.member, src, reported, pem)
			if err := res.st.Save(); err != nil {
				return fmt.Errorf("%s upgraded, but recording it in state failed (the next plan may show it as drifted): %w",
					res.member.Hostname, err)
			}
			return nil
		},
	}
}

// upgradeExecTimeoutSec bounds how long mayfly watches the in-guest upgrade
// before falling through to verification. It is not a budget for the upgrade
// itself — nothing cancels the guest process when this expires — just how long
// watching stays more informative than polling the node's status.
const upgradeExecTimeoutSec = 600

func describeTarget(version string) string {
	if version == "" {
		return "the pinned binary"
	}
	return version
}

func describeVersion(reported string) string {
	if reported == "" {
		return "unknown version"
	}
	return strings.TrimSpace(firstLine(reported))
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// execTimeoutSec is node exec's default budget for one command or script.
const execTimeoutSec = 300

// maxScriptBytes caps a --script. The guest agent takes a script in 32 KiB
// file-writes, so this is ~32 round trips — far beyond any operator script,
// and small enough that a wrong file (a binary, a log) fails fast.
const maxScriptBytes = 1 << 20

func nodeExecCmd() *cli.Command {
	return &cli.Command{
		Name:  "exec",
		Usage: "Run a command or a local script on a node (guest agent or SSH — whichever its pool uses)",
		ArgsUsage: "<name> -- <command> [args...]\n" +
			"   mayfly node exec --script <file|-> <name> [-- script-args...]",
		Description: "With --script, mayfly reads the script itself (from a file, or stdin for \"-\") and delivers\n" +
			"its bytes to the node as file contents, never on a command line, so no local or remote shell\n" +
			"re-parses its quotes. Interpreter: sh on linux/darwin (a #! line is honoured); on windows,\n" +
			".cmd/.bat run under cmd.exe and everything else (.ps1, other names, stdin) under\n" +
			"powershell -File. Arguments after -- become the script's positional parameters.\n\n" +
			"The registry-cache and monitoring VMs are reachable too, by hostname or by the names\n" +
			"\"registry-cache\" and \"monitoring\".",
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "script", Usage: "run this local script file on the node (\"-\" reads it from stdin)"},
			&cli.IntFlag{Name: "timeout", Value: execTimeoutSec, Usage: "seconds to wait for the command or script"},
		},
		Action: func(ctx context.Context, c *cli.Command) error {
			args := c.Args().Slice()
			script := c.String("script")
			if script == "" && len(args) < 2 {
				return fmt.Errorf("usage: mayfly node exec <name> -- <command> [args...]\n" +
					"   or: mayfly node exec --script <file|-> <name> [-- script-args...]")
			}
			if script != "" && len(args) < 1 {
				return fmt.Errorf("usage: mayfly node exec --script <file|-> <name> [-- script-args...]")
			}
			ex, err := execTarget(args[0])
			if err != nil {
				return err
			}
			if script == "" {
				return report(ex.Exec(ctx, args[1:], c.Int("timeout")))
			}
			s, err := readScript(script, os.Stdin)
			if err != nil {
				return err
			}
			s.Args = args[1:]
			return report(ex.ExecScript(ctx, s, c.Int("timeout")))
		},
	}
}

// execTarget is resolveExecTarget, swappable so tests can drive the command
// without a fleet.
var execTarget = resolveExecTarget

// resolveExecTarget opens a channel to a pool member or, failing that, to one
// of the mayfly-managed service VMs (registry cache, monitoring). Only exec
// reaches the service VMs: the other node commands drive ephemerd, which those
// VMs do not run.
func resolveExecTarget(name string) (engine.NodeExecutor, error) {
	res, err := resolveNode(name)
	if err == nil {
		return res.exec, nil
	}
	st, serr := state.Load()
	if serr != nil {
		return nil, err
	}
	target, ok := engine.ResolveServiceVMTarget(st, name)
	if !ok {
		return nil, err
	}
	cfg, cerr := config.LoadForNode()
	if cerr != nil {
		return nil, cerr
	}
	return engine.NewNodeExecutor(cfg, target)
}

// readScript loads a --script argument: a file path, or "-" for stdin. The
// bytes are returned untouched; any interpreter-specific line-ending or
// encoding fix-up happens in the engine, once the node's OS is known.
func readScript(src string, stdin *os.File) (engine.NodeScript, error) {
	var (
		body []byte
		err  error
		name string
	)
	if src == "-" {
		// A terminal on stdin means nothing was piped: reading would block
		// waiting for the operator to type a script.
		if fi, serr := stdin.Stat(); serr == nil && fi.Mode()&os.ModeCharDevice != 0 {
			return engine.NodeScript{}, fmt.Errorf("--script -: pipe or redirect the script on stdin")
		}
		body, err = io.ReadAll(io.LimitReader(stdin, maxScriptBytes+1))
	} else {
		name = src
		var f *os.File
		if f, err = os.Open(src); err == nil {
			body, err = io.ReadAll(io.LimitReader(f, maxScriptBytes+1))
			_ = f.Close()
		}
	}
	if err != nil {
		return engine.NodeScript{}, fmt.Errorf("read script: %w", err)
	}
	if len(body) > maxScriptBytes {
		return engine.NodeScript{}, fmt.Errorf("script %s is over the %d KiB limit", scriptLabel(src), maxScriptBytes>>10)
	}
	if len(bytes.TrimSpace(body)) == 0 {
		return engine.NodeScript{}, fmt.Errorf("script %s is empty", scriptLabel(src))
	}
	return engine.NodeScript{Body: body, Name: name}, nil
}

func scriptLabel(src string) string {
	if src == "-" {
		return "on stdin"
	}
	return src
}

// nodeReconfigureCmd re-renders ONE node's ephemerd config from its pool spec
// and restarts ephemerd, without a full-fleet `mayfly apply`. apply is the only
// other thing that renders node config, but it also refreshes drift, checks
// templates, runs the VLAN preflight, and does VM lifecycle work — so a single
// down node in the fleet, or a VLAN-aware-bridge preflight failure, blocks it
// entirely. This command is the escape hatch: config-tier only, one node, no
// plan. It replaces hand-editing /var/lib/ephemerd/config.toml with sed and
// bouncing the service by hand.
func nodeReconfigureCmd() *cli.Command {
	return &cli.Command{
		Name:      "reconfigure",
		Usage:     "Re-render a node's ephemerd config from its pool and restart ephemerd (no full apply)",
		ArgsUsage: "<name>",
		Flags: []cli.Flag{
			&cli.BoolFlag{Name: "no-restart", Usage: "write the re-rendered config but leave ephemerd running the old one (takes effect on its next restart)"},
		},
		Action: func(ctx context.Context, c *cli.Command) error {
			name := c.Args().First()
			if name == "" {
				return fmt.Errorf("node name required")
			}
			e, err := newEngine()
			if err != nil {
				return err
			}
			pool, member, ok := e.St.FindMemberByName(name)
			if !ok {
				return fmt.Errorf("no node named %q in state", name)
			}
			restart := !c.Bool("no-restart")
			if err := e.ReconfigureMember(ctx, pool.Name, member, restart); err != nil {
				return err
			}
			// Persist the refreshed ConfigHash so a later `mayfly plan` sees no
			// spurious config drift for this node.
			if err := e.St.Save(); err != nil {
				return err
			}
			if restart {
				fmt.Fprintf(os.Stderr, "%s: config re-rendered and ephemerd restarted\n", member.Hostname)
			} else {
				fmt.Fprintf(os.Stderr, "%s: config re-rendered and staged; ephemerd still running the old config until its next restart\n", member.Hostname)
			}
			return nil
		},
	}
}

// doctorCheckTimeoutSec bounds each individual probe in `node doctor`.
const doctorCheckTimeoutSec = 60

func nodeDoctorCmd() *cli.Command {
	return &cli.Command{
		Name:      "doctor",
		Usage:     "Run a health + isolation battery on a node (PASS/FAIL per check)",
		ArgsUsage: "<name>",
		Action: func(ctx context.Context, c *cli.Command) error {
			res, err := resolveNode(c.Args().First())
			if err != nil {
				return err
			}
			return runDoctor(ctx, res)
		},
	}
}

// nodeResizeCmd changes a node's CPU, memory, or disk. Resources take effect on
// next boot, so it power-cycles the node: best-effort drain, graceful shutdown,
// apply the change, start, and confirm ephemerd comes back serving. Only
// hypervisor-backed nodes have resizable resources; an SSH-enrolled Mac or
// bare-metal box is refused. A disk request smaller than the recorded size is
// refused outright — Proxmox cannot shrink a disk.
func nodeResizeCmd() *cli.Command {
	return &cli.Command{
		Name:      "resize",
		Usage:     "Resize a node's CPU/memory/disk (power-cycles it; hypervisor nodes only)",
		ArgsUsage: "<node>",
		Flags: []cli.Flag{
			&cli.IntFlag{Name: "cores", Usage: "target vCPU count"},
			&cli.IntFlag{Name: "memory-mb", Usage: "target memory in MB"},
			&cli.IntFlag{Name: "disk-gb", Usage: "target root disk size in GB (grow only; a smaller value is refused)"},
		},
		Action: func(ctx context.Context, c *cli.Command) error {
			res, err := resolveNode(c.Args().First())
			if err != nil {
				return err
			}
			if !c.IsSet("cores") && !c.IsSet("memory-mb") && !c.IsSet("disk-gb") {
				return fmt.Errorf("resize: set at least one of --cores, --memory-mb, --disk-gb")
			}
			inst, err := res.instance(ctx)
			if err != nil {
				if errors.Is(err, engine.ErrNoInstance) {
					return fmt.Errorf("resize is only supported on hypervisor-backed nodes: %w", err)
				}
				return err
			}

			// Proxmox writes cores and memory in one config call, so fill any
			// unset dimension from the node's recorded value rather than zeroing it.
			spec := engine.ResizeSpec{}
			if c.IsSet("cores") || c.IsSet("memory-mb") {
				spec.SetResources = true
				spec.Cores = res.member.Cores
				spec.MemoryMB = res.member.MemoryMB
				if c.IsSet("cores") {
					spec.Cores = c.Int("cores")
				}
				if c.IsSet("memory-mb") {
					spec.MemoryMB = c.Int("memory-mb")
				}
			}
			// Guard against a disk shrink using the LIVE size, not recorded
			// state — an out-of-band Proxmox resize (the very reason this command
			// exists) drifts recorded DiskGB, so the recorded value could falsely
			// refuse a legit grow or let a real shrink through. Read live only
			// when a disk resize is actually requested.
			currentDiskGB := res.member.DiskGB
			if c.IsSet("disk-gb") {
				spec.ResizeDisk = true
				spec.DiskGB = c.Int("disk-gb")
				live, derr := inst.DiskGB(ctx)
				if derr != nil {
					return fmt.Errorf("resize: cannot read current disk size to guard against a shrink: %w", derr)
				}
				currentDiskGB = live
			}

			applied, err := engine.ResizeNode(ctx, inst, res.member.OS, spec, currentDiskGB, nodeLogf(res.member.Hostname))
			// Persist whatever actually reached the hypervisor — even if a later
			// step (start, guest-agent wait, serving check) failed — so `node
			// list` and the next plan's drift check never show stale resources
			// for a VM that was already reconfigured.
			if applied.ResourcesApplied {
				res.member.Cores = applied.Cores
				res.member.MemoryMB = applied.MemoryMB
			}
			if applied.DiskApplied {
				res.member.DiskGB = applied.DiskGB
			}
			if applied.ResourcesApplied || applied.DiskApplied {
				if saveErr := res.st.Save(); saveErr != nil {
					if err == nil {
						err = saveErr
					} else {
						fmt.Fprintf(os.Stderr, "%s: warning: could not persist resized state: %v\n", res.member.Hostname, saveErr)
					}
				}
			}
			return err
		},
	}
}

// runDoctor executes each check in the OS-appropriate battery, prints a
// PASS/FAIL/SKIP line with a one-line detail (and a remediation hint on
// failure), and returns a non-nil error when any hard check failed. A check
// whose command could not be run at all (unreachable node, dead channel) counts
// as a failure, not a skip — that is exactly the condition doctor exists to
// surface.
func runDoctor(ctx context.Context, res resolvedNode) error {
	fmt.Fprintf(os.Stderr, "doctor: %s (%s/%s, %s)\n",
		res.member.Hostname, res.member.OS, res.member.Arch, res.target.Transport)
	failed := 0
	for _, ck := range engine.NodeDoctorChecks(res.member.OS) {
		if ck.NA != "" {
			fmt.Printf("SKIP %-18s %s\n", ck.Name, ck.NA)
			continue
		}
		out, err := res.exec.Exec(ctx, ck.Argv, doctorCheckTimeoutSec)
		if err != nil {
			failed++
			fmt.Printf("FAIL %-18s could not run: %v\n", ck.Name, err)
			fmt.Printf("       hint: %s\n", ck.Hint)
			continue
		}
		pass, detail := ck.Eval(out)
		if pass {
			fmt.Printf("PASS %-18s %s\n", ck.Name, detail)
			continue
		}
		failed++
		fmt.Printf("FAIL %-18s %s\n", ck.Name, detail)
		fmt.Printf("       hint: %s\n", ck.Hint)
	}
	if failed > 0 {
		return fmt.Errorf("%d check(s) failed on %s", failed, res.member.Hostname)
	}
	fmt.Fprintf(os.Stderr, "doctor: %s healthy\n", res.member.Hostname)
	return nil
}

// nodeRebootCmd gracefully reboots a node. On a hypervisor node it drains, then
// graceful-shutdown-and-starts the VM. On an SSH-enrolled node there is no
// hypervisor power channel, so it falls back to an in-guest `sudo reboot`.
func nodeRebootCmd() *cli.Command {
	return &cli.Command{
		Name:      "reboot",
		Usage:     "Gracefully reboot a node (drain, then power-cycle)",
		ArgsUsage: "<node>",
		Flags: []cli.Flag{
			&cli.BoolFlag{
				Name:  "require-drain",
				Usage: "refuse to reboot when the node cannot be drained (e.g. its guest agent is dead) instead of interrupting running jobs",
			},
		},
		Action: func(ctx context.Context, c *cli.Command) error {
			res, err := resolveNode(c.Args().First())
			if err != nil {
				return err
			}
			inst, err := res.instance(ctx)
			if err != nil {
				if errors.Is(err, engine.ErrNoInstance) {
					// Bare metal / darwin: no hypervisor to power-cycle. Drain
					// first so a running job (e.g. a Mac CI leg) is not killed,
					// then reboot from inside. `reboot` tears the box down and so
					// drops the SSH connection — which surfaces as a transport
					// error even though the reboot succeeded — so the dropped
					// connection is reported, not treated as a failure.
					fmt.Fprintf(os.Stderr, "%s: no hypervisor power channel; draining then rebooting in-guest\n", res.member.Hostname)
					if dres, derr := res.exec.Exec(ctx, engine.EphemerdArgv(res.member.OS, "drain"), sshDrainTimeoutSec); derr != nil || dres.ExitCode != 0 {
						problem := fmt.Sprintf("drain exited %d", dres.ExitCode)
						if derr != nil {
							problem = fmt.Sprintf("cannot drain (%v)", derr)
						}
						if c.Bool("require-drain") {
							return fmt.Errorf("%s: %s; refusing to reboot because --require-drain is set (running jobs would be interrupted)",
								res.member.Hostname, problem)
						}
						fmt.Fprintf(os.Stderr, "%s: %s; running jobs will be interrupted\n", res.member.Hostname, problem)
					}
					if _, rerr := res.exec.Exec(ctx, []string{"reboot"}, 60); rerr != nil {
						fmt.Fprintf(os.Stderr, "%s: reboot issued; SSH connection dropped as the node goes down (%v)\n", res.member.Hostname, rerr)
					}
					return nil
				}
				return err
			}
			return engine.RebootNode(ctx, inst, res.member.OS, engine.RebootOptions{RequireDrain: c.Bool("require-drain")}, nodeLogf(res.member.Hostname))
		},
	}
}

// nodePowerCmd is the hard, guest-agent-independent power lever: it goes
// straight to the hypervisor, bypassing the guest agent, so it recovers a node
// whose guest agent has wedged and cannot be reached to drain or shut down
// gracefully. `reset` is a hard Stop+Start.
func nodePowerCmd() *cli.Command {
	return &cli.Command{
		Name:      "power",
		Usage:     "Hard power control straight to the hypervisor (stop|start|reset) — recovers a hung guest",
		ArgsUsage: "{stop|start|reset} <node>",
		Action: func(ctx context.Context, c *cli.Command) error {
			args := c.Args().Slice()
			if len(args) != 2 {
				return fmt.Errorf("usage: mayfly node power {stop|start|reset} <node>")
			}
			action := engine.PowerAction(args[0])
			switch action {
			case engine.PowerStop, engine.PowerStart, engine.PowerReset:
			default:
				return fmt.Errorf("unknown power action %q (want stop, start, or reset)", args[0])
			}
			res, err := resolveNode(args[1])
			if err != nil {
				return err
			}
			inst, err := res.instance(ctx)
			if err != nil {
				if errors.Is(err, engine.ErrNoInstance) {
					return fmt.Errorf("power control is only supported on hypervisor-backed nodes: %w", err)
				}
				return err
			}
			return engine.PowerNode(ctx, inst, action, nodeLogf(res.member.Hostname))
		},
	}
}

// sshDrainTimeoutSec bounds the best-effort in-guest drain before an SSH-node
// reboot, matching `mayfly node drain`.
const sshDrainTimeoutSec = 600

// nodeLogf returns a progress logger that prefixes each line with the node's
// hostname and writes to stderr, keeping stdout clean for command output.
func nodeLogf(host string) func(string, ...any) {
	return func(format string, args ...any) {
		fmt.Fprintf(os.Stderr, host+": "+format+"\n", args...)
	}
}

func jobsCmd() *cli.Command {
	return &cli.Command{
		Name:      "jobs",
		Usage:     "Show ephemerd jobs on a node (passes args through to `ephemerd jobs`)",
		ArgsUsage: "<name> [jobs subcommand args...]",
		Action: func(ctx context.Context, c *cli.Command) error {
			args := c.Args().Slice()
			if len(args) < 1 {
				return fmt.Errorf("usage: mayfly jobs <name> [args...]")
			}
			return runOnNode(ctx, args[0], 120, func(_ *config.Config, m *state.Member) ([]string, error) {
				return engine.EphemerdArgv(m.OS, append([]string{"jobs"}, args[1:]...)...), nil
			})
		},
	}
}

// resolvedNode bundles everything a node subcommand needs after lookup.
type resolvedNode struct {
	cfg    *config.Config
	st     *state.Store
	pool   *state.Pool
	member *state.Member
	target engine.NodeTarget
	exec   engine.NodeExecutor
}

// spec returns the node's pool spec from config, and whether it is still
// configured (a node can outlive its pool's entry mid-teardown).
func (r resolvedNode) spec() (config.PoolSpec, bool) {
	raw, ok := r.cfg.Pools[r.pool.Name]
	if !ok {
		return config.PoolSpec{}, false
	}
	return raw.Resolved(r.cfg.Defaults), true
}

// resolveNode finds a node by name and opens the right channel to it.
func resolveNode(name string) (resolvedNode, error) {
	if name == "" {
		return resolvedNode{}, fmt.Errorf("node name required")
	}
	// Single-node commands (status/exec/upgrade/drain) authenticate to
	// nothing, so they use the scoped loader: it validates structure and any
	// proxmox/transport the target needs, but does not require every pool's
	// GitHub App / token credentials.
	cfg, err := config.LoadForNode()
	if err != nil {
		return resolvedNode{}, err
	}
	st, err := state.Load()
	if err != nil {
		return resolvedNode{}, err
	}
	pool, member, ok := st.FindMemberByName(name)
	if !ok {
		return resolvedNode{}, fmt.Errorf("no node named %q in state", name)
	}
	target, ex, err := engine.NodeExecutorFor(cfg, pool, member)
	if err != nil {
		return resolvedNode{}, err
	}
	return resolvedNode{cfg: cfg, st: st, pool: pool, member: member, target: target, exec: ex}, nil
}

// instance resolves the provider.Instance backing this node, for the lifecycle
// commands (resize, power) that drive the VM itself rather than run a command
// inside it. SSH-enrolled nodes have no instance and surface engine.ErrNoInstance.
func (r resolvedNode) instance(ctx context.Context) (provider.Instance, error) {
	_, inst, err := engine.NodeInstanceFor(ctx, r.cfg, r.pool, r.member)
	return inst, err
}

// runOnNode is the shared body of every simple node subcommand: resolve, build
// the argv from the member, run, print.
func runOnNode(ctx context.Context, name string, timeoutSec int, build func(*config.Config, *state.Member) ([]string, error)) error {
	res, err := resolveNode(name)
	if err != nil {
		return err
	}
	argv, err := build(res.cfg, res.member)
	if err != nil {
		return err
	}
	return report(res.exec.Exec(ctx, argv, timeoutSec))
}

// report prints a command's output and turns a non-zero exit into an error.
func report(res engine.NodeExecResult, err error) error {
	if err != nil {
		return err
	}
	writeStream(os.Stdout, res.Stdout)
	writeStream(os.Stderr, res.Stderr)
	if res.ExitCode != 0 {
		return fmt.Errorf("exit code %d", res.ExitCode)
	}
	return nil
}

// writeStream prints captured output, adding the trailing newline the guest
// agent channel tends to drop.
func writeStream(w io.Writer, s string) {
	if s == "" {
		return
	}
	fmt.Fprint(w, s)
	if s[len(s)-1] != '\n' {
		fmt.Fprintln(w)
	}
}
