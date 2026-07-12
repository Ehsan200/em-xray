// Command emx is the CLI + TUI front-end and the daemon entrypoint for the
// em-xray master-dialer / subscription system.
package main

import (
	"fmt"
	"os"

	"github.com/ehsan200/em-xray/daemon"
	"github.com/spf13/cobra"
)

// version is stamped at build time via -ldflags "-X main.version=...".
var version = "dev"

func main() {
	daemon.SetVersion(version)
	if err := rootCmd().Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "emx:", err)
		os.Exit(1)
	}
}

func rootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:           "emx",
		Short:         "master xray dialer + subscriptions",
		SilenceUsage:  true,
		SilenceErrors: true,
		// Bare `emx` on a terminal opens the interactive menu.
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) > 0 {
				return fmt.Errorf("unknown command %q", args[0])
			}
			return runMenu(cmd, "main")
		},
	}
	root.AddCommand(
		startCmd(),
		stopCmd(),
		restartCmd(),
		statusCmd(),
		daemonRunCmd(), // hidden: the detached daemon process itself
		versionCmd(),
		templateCmd(),
		entryCmd(),
		inboundCmd(),
		subCmd(),
		winnerCmd(),
		updateCmd(),
		systemdCmd(),
		uiCmd(),
	)
	return root
}

func uiCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "ui",
		Short: "open the interactive menu",
		RunE:  func(cmd *cobra.Command, _ []string) error { return runMenu(cmd, "main") },
	}
}

func versionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "print emx and embedded xray versions",
		RunE: func(cmd *cobra.Command, _ []string) error {
			fmt.Fprintf(cmd.OutOrStdout(), "emx %s\nxray %s\n", version, xrayEmbeddedVersion())
			return nil
		},
	}
}
