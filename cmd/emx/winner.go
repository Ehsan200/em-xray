package main

import (
	"context"
	"fmt"
	"text/tabwriter"

	emxv1 "github.com/ehsan200/em-xray/api/emxv1"
	"github.com/spf13/cobra"
)

func winnerCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "winner",
		Short:   "show the current fastest node per master",
		Aliases: []string{"winners"},
		RunE: func(cmd *cobra.Command, _ []string) error {
			return withClient(cmd, func(ctx context.Context, cl emxv1.DaemonClient) error {
				reply, err := cl.Winners(ctx, &emxv1.Empty{})
				if err != nil {
					return err
				}
				if len(reply.Winners) == 0 {
					fmt.Fprintln(cmd.OutOrStdout(), "no masters running (start an inbound targeting a master)")
					return nil
				}
				tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
				fmt.Fprintln(tw, "MASTER\tMEMBERS\tFASTEST NODE")
				for _, w := range reply.Winners {
					node := w.Node
					switch {
					case w.Members == 0:
						node = "BLOCKED (pool empty)"
					case node == "":
						node = "(selecting…)"
					}
					fmt.Fprintf(tw, "%s\t%d\t%s\n", w.Master, w.Members, node)
				}
				return tw.Flush()
			})
		},
	}
}
