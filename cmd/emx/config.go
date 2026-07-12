package main

import (
	"context"
	"fmt"
	"os"

	emxv1 "github.com/ehsan200/em-xray/api/emxv1"
	"github.com/spf13/cobra"
)

func configCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "config",
		Short: "backup / restore the full config (inbounds, entries, subscriptions)",
	}
	c.AddCommand(configExportCmd(), configImportCmd())
	return c
}

func configExportCmd() *cobra.Command {
	var out string
	c := &cobra.Command{
		Use: "export", Short: "export the full config as JSON (stdout, or --out FILE)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return withClient(cmd, func(ctx context.Context, cl emxv1.DaemonClient) error {
				reply, err := cl.ExportConfig(ctx, &emxv1.Empty{})
				if err != nil {
					return err
				}
				if out == "" {
					fmt.Fprintln(cmd.OutOrStdout(), reply.Json)
					return nil
				}
				if err := os.WriteFile(out, []byte(reply.Json), 0o600); err != nil {
					return err
				}
				fmt.Fprintf(cmd.OutOrStdout(), "wrote %s\n", out)
				return nil
			})
		},
	}
	c.Flags().StringVarP(&out, "out", "o", "", "write to this file instead of stdout")
	return c
}

func configImportCmd() *cobra.Command {
	var replace bool
	c := &cobra.Command{
		Use: "import <file>", Short: "restore a config backup (merges by default; --replace wipes first)",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			data, err := os.ReadFile(args[0])
			if err != nil {
				return err
			}
			return withClient(cmd, func(ctx context.Context, cl emxv1.DaemonClient) error {
				reply, err := cl.ImportConfig(ctx, &emxv1.ImportRequest{Json: string(data), Replace: replace})
				if err != nil {
					return err
				}
				out := cmd.OutOrStdout()
				fmt.Fprintf(out, "imported: %d inbounds, %d entries, %d subscriptions\n",
					reply.Inbounds, reply.Entries, reply.Subs)
				for _, s := range reply.Skipped {
					fmt.Fprintf(out, "  skipped (name exists): %s\n", s)
				}
				return nil
			})
		},
	}
	c.Flags().BoolVar(&replace, "replace", false, "wipe existing config before importing")
	return c
}
