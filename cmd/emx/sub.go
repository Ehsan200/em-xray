package main

import (
	"context"
	"fmt"
	"strconv"
	"text/tabwriter"
	"time"

	emxv1 "github.com/gravisun/em-xray/api/emxv1"
	"github.com/spf13/cobra"
)

func subCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "sub",
		Short: "manage subscriptions (node pools for masters)",
		RunE:  func(cmd *cobra.Command, _ []string) error { return runMenu(cmd, "sub") },
	}
	c.AddCommand(
		subAddCmd(), subListCmd(), subRemoveCmd(),
		subEnableCmd(true), subEnableCmd(false),
		subRefreshCmd(), subNodesCmd(),
		subNodeDisableCmd(true), subNodeDisableCmd(false),
		subRenameCmd(),
	)
	return c
}

func subRenameCmd() *cobra.Command {
	return &cobra.Command{
		Use: "rename <id> <new-name>", Short: "rename a subscription (cascades into masters' dialers)",
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := parseID(args[0])
			if err != nil {
				return err
			}
			return withClient(cmd, func(ctx context.Context, cl emxv1.DaemonClient) error {
				if _, err := cl.SubRename(ctx, &emxv1.RenameRequest{Id: id, NewName: args[1]}); err != nil {
					return err
				}
				fmt.Fprintln(cmd.OutOrStdout(), "renamed")
				return nil
			})
		},
	}
}

func subAddCmd() *cobra.Command {
	var interval, cap int
	var ua string
	c := &cobra.Command{
		Use:   "add <name> <url>",
		Short: "add a subscription and fetch it now",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return withClient(cmd, func(ctx context.Context, cl emxv1.DaemonClient) error {
				reply, err := cl.SubAdd(ctx, &emxv1.SubAddRequest{
					Name: args[0], Url: args[1], UserAgent: ua,
					IntervalSec: int32(interval), NodeCap: int32(cap),
				})
				if err != nil {
					return err
				}
				sub := reply.Sub
				fmt.Fprintf(cmd.OutOrStdout(), "added subscription %q (id %d): %d nodes, %d active\n",
					sub.Name, sub.Id, sub.NodeCount, sub.ActiveCount)
				if sub.LastError != "" {
					fmt.Fprintf(cmd.OutOrStdout(), "  fetch error: %s\n", sub.LastError)
				}
				return nil
			})
		},
	}
	c.Flags().IntVar(&interval, "interval", 0, "refresh interval seconds (0 = 12h default)")
	c.Flags().IntVar(&cap, "cap", 0, "max active nodes (0 = 30 default)")
	c.Flags().StringVar(&ua, "ua", "", "User-Agent (0 = v2rayN default)")
	return c
}

func subListCmd() *cobra.Command {
	return &cobra.Command{
		Use: "ls", Short: "list subscriptions", Aliases: []string{"list"},
		RunE: func(cmd *cobra.Command, _ []string) error {
			return withClient(cmd, func(ctx context.Context, cl emxv1.DaemonClient) error {
				reply, err := cl.SubList(ctx, &emxv1.Empty{})
				if err != nil {
					return err
				}
				tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
				fmt.Fprintln(tw, "ID\tNAME\tENABLED\tNODES\tACTIVE\tERROR")
				for _, s := range reply.Subs {
					fmt.Fprintf(tw, "%d\t%s\t%v\t%d\t%d\t%s\n", s.Id, s.Name, s.Enabled, s.NodeCount, s.ActiveCount, s.LastError)
				}
				return tw.Flush()
			})
		},
	}
}

func subRemoveCmd() *cobra.Command {
	return &cobra.Command{
		Use: "rm <id>", Short: "remove a subscription", Aliases: []string{"remove", "del"},
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := parseID(args[0])
			if err != nil {
				return err
			}
			return withClient(cmd, func(ctx context.Context, cl emxv1.DaemonClient) error {
				if _, err := cl.SubRemove(ctx, &emxv1.IdRequest{Id: id}); err != nil {
					return err
				}
				fmt.Fprintln(cmd.OutOrStdout(), "removed")
				return nil
			})
		},
	}
}

func subEnableCmd(enable bool) *cobra.Command {
	use, verb := "enable <id>", "enabled"
	if !enable {
		use, verb = "disable <id>", "disabled"
	}
	return &cobra.Command{
		Use: use, Short: verb[:len(verb)-1] + " a subscription", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := parseID(args[0])
			if err != nil {
				return err
			}
			return withClient(cmd, func(ctx context.Context, cl emxv1.DaemonClient) error {
				if _, err := cl.SubSetEnabled(ctx, &emxv1.SetEnabledRequest{Id: id, Enabled: enable}); err != nil {
					return err
				}
				fmt.Fprintln(cmd.OutOrStdout(), verb)
				return nil
			})
		},
	}
}

func subRefreshCmd() *cobra.Command {
	return &cobra.Command{
		Use: "refresh [id]", Short: "refresh one subscription (or all if omitted)",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			var id uint32
			if len(args) == 1 {
				v, err := parseID(args[0])
				if err != nil {
					return err
				}
				id = v
			}
			return withClientTimeout(cmd, 60*time.Second, func(ctx context.Context, cl emxv1.DaemonClient) error {
				reply, err := cl.SubRefresh(ctx, &emxv1.SubRefreshRequest{Id: id})
				if err != nil {
					return err
				}
				if id == 0 {
					fmt.Fprintln(cmd.OutOrStdout(), "refreshed all")
				} else {
					fmt.Fprintf(cmd.OutOrStdout(), "refreshed: %d nodes\n", reply.Nodes)
				}
				return nil
			})
		},
	}
}

func subNodesCmd() *cobra.Command {
	return &cobra.Command{
		Use: "nodes <id>", Short: "list a subscription's nodes", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := parseID(args[0])
			if err != nil {
				return err
			}
			return withClient(cmd, func(ctx context.Context, cl emxv1.DaemonClient) error {
				reply, err := cl.SubNodes(ctx, &emxv1.IdRequest{Id: id})
				if err != nil {
					return err
				}
				tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
				fmt.Fprintln(tw, "FINGERPRINT\tNAME\tACTIVE\tDISABLED\tLATENCY")
				for _, n := range reply.Nodes {
					lat := "-"
					if n.LatencyMs > 0 {
						lat = strconv.Itoa(int(n.LatencyMs)) + "ms"
					}
					fmt.Fprintf(tw, "%s\t%s\t%v\t%v\t%s\n", n.Fingerprint, n.Name, n.Active, n.Disabled, lat)
				}
				return tw.Flush()
			})
		},
	}
}

func subNodeDisableCmd(disable bool) *cobra.Command {
	use, verb := "node-disable <subid> <fingerprint>", "disabled"
	if !disable {
		use, verb = "node-enable <subid> <fingerprint>", "enabled"
	}
	return &cobra.Command{
		Use: use, Short: "toggle a single node (durable, survives refresh)", Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := parseID(args[0])
			if err != nil {
				return err
			}
			return withClient(cmd, func(ctx context.Context, cl emxv1.DaemonClient) error {
				if _, err := cl.SubSetNodeDisabled(ctx, &emxv1.SubNodeDisabledRequest{
					SubId: id, Fingerprint: args[1], Disabled: disable,
				}); err != nil {
					return err
				}
				fmt.Fprintln(cmd.OutOrStdout(), "node "+verb)
				return nil
			})
		},
	}
}

// ---- small CLI helpers -----------------------------------------------------

func parseID(s string) (uint32, error) {
	v, err := strconv.ParseUint(s, 10, 32)
	if err != nil {
		return 0, fmt.Errorf("bad id %q", s)
	}
	return uint32(v), nil
}

func withClient(cmd *cobra.Command, fn func(context.Context, emxv1.DaemonClient) error) error {
	return withClientTimeout(cmd, 10*time.Second, fn)
}

func withClientTimeout(cmd *cobra.Command, d time.Duration, fn func(context.Context, emxv1.DaemonClient) error) error {
	ctx, cancel := context.WithTimeout(cmd.Context(), d)
	defer cancel()
	client, conn, err := dialReady(ctx)
	if err != nil {
		return errDaemon(err)
	}
	defer conn.Close()
	return fn(ctx, client)
}