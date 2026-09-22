package main

import (
	"context"
	"fmt"
	"text/tabwriter"
	"time"

	emxv1 "github.com/ehsan200/em-xray/api/emxv1"
	"github.com/ehsan200/em-xray/internal/paths"
	"github.com/spf13/cobra"
)

func templateCmd() *cobra.Command {
	c := &cobra.Command{Use: "template", Short: "inbound config templates"}
	c.AddCommand(&cobra.Command{
		Use:     "ls",
		Short:   "list built-in templates",
		Aliases: []string{"list"},
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx, cancel := context.WithTimeout(cmd.Context(), 3*time.Second)
			defer cancel()
			client, conn, err := dialReady(ctx)
			if err != nil {
				return errDaemon(err)
			}
			defer conn.Close()
			reply, err := client.TemplateList(ctx, &emxv1.Empty{})
			if err != nil {
				return err
			}
			tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "NAME\tPROTOCOL\tNETWORK\tSECURITY\tDESCRIPTION")
			for _, t := range reply.Templates {
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", t.Name, t.Protocol, t.Network, t.Security, t.Description)
			}
			return tw.Flush()
		},
	})
	return c
}

// errDaemon wraps a dial failure with a hint to start the daemon.
func errDaemon(err error) error {
	p := paths.Default()
	return fmt.Errorf("cannot reach daemon at %s (start or restart it using the same user/scope as this command): %w", p.Socket(), err)
}
