package main

import (
	"context"
	"fmt"
	"runtime"
	"time"

	"github.com/ehsan200/em-xray/daemon"
	"github.com/ehsan200/em-xray/internal/paths"
	"github.com/ehsan200/em-xray/internal/selfupdate"
	"github.com/spf13/cobra"
)

func updateCmd() *cobra.Command {
	var checkOnly, noRestart bool
	c := &cobra.Command{
		Use:   "update",
		Short: "check for a newer release and self-update the binary",
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx, cancel := context.WithTimeout(cmd.Context(), 60*time.Second)
			defer cancel()
			out := cmd.OutOrStdout()

			rel, err := selfupdate.Latest(ctx)
			if err != nil {
				return fmt.Errorf("check for updates: %w", err)
			}
			if !selfupdate.Newer(version, rel.Tag) {
				fmt.Fprintf(out, "up to date (%s)\n", version)
				return nil
			}
			fmt.Fprintf(out, "update available: %s → %s\n", version, rel.Tag)
			if checkOnly {
				fmt.Fprintln(out, "run `emx update` to install")
				return nil
			}

			url, ok := rel.AssetURL(runtime.GOOS, runtime.GOARCH)
			if !ok {
				return fmt.Errorf("no %s/%s build in release %s", runtime.GOOS, runtime.GOARCH, rel.Tag)
			}
			fmt.Fprintln(out, "downloading…")
			if err := selfupdate.Apply(ctx, url); err != nil {
				return err
			}
			fmt.Fprintf(out, "updated to %s\n", rel.Tag)

			// Restart the running daemon so it executes the new binary.
			_, alive := daemon.RunningPID(paths.Default())
			if alive && !noRestart {
				fmt.Fprintln(out, "restarting daemon…")
				if err := restartDaemon(cmd); err != nil {
					return fmt.Errorf("updated, but daemon restart failed (run `emx restart`): %w", err)
				}
				fmt.Fprintln(out, "daemon restarted on the new version")
			} else if alive {
				fmt.Fprintln(out, "restart the daemon to run the new version:  emx restart")
			}
			return nil
		},
	}
	c.Flags().BoolVar(&checkOnly, "check", false, "only report whether an update is available")
	c.Flags().BoolVar(&noRestart, "no-restart", false, "don't restart the daemon after updating")
	return c
}
