package main

import "github.com/urfave/cli/v3"

func rootCmd() *cli.Command {
	return &cli.Command{
		Name:  "mayfly",
		Usage: "Declaratively manage fleets of ephemeral GitHub Actions runners on Proxmox",
		Commands: []*cli.Command{
			planCmd(),
			applyCmd(),
			destroyCmd(),
			nodeCmd(),
			jobsCmd(),
			webhooksCmd(),
			templateCmd(),
			modelServerCmd(),
		},
	}
}

func nodeCmd() *cli.Command {
	return &cli.Command{
		Name:  "node",
		Usage: "Operational commands against individual runner nodes",
		Commands: []*cli.Command{
			nodeListCmd(),
			nodeStatusCmd(),
			nodeLogsCmd(),
			nodeDoctorCmd(),
			nodeDrainCmd(),
			nodeUpgradeCmd(),
			nodeReconfigureCmd(),
			nodeResizeCmd(),
			nodeRebootCmd(),
			nodePowerCmd(),
			nodeExecCmd(),
		},
	}
}

func templateCmd() *cli.Command {
	return &cli.Command{
		Name:  "template",
		Usage: "Manage the base Proxmox template",
		Commands: []*cli.Command{
			templateSyncCmd(),
		},
	}
}
