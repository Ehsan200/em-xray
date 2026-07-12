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

func entryCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "entry",
		Short: "manage outbounds (and masters)",
		RunE:  func(cmd *cobra.Command, _ []string) error { return runMenu(cmd, "entry") },
	}
	c.AddCommand(entryAddCmd(), entryListCmd(), entryRemoveCmd(), entryRenameCmd())
	return c
}

func entryRenameCmd() *cobra.Command {
	return &cobra.Command{
		Use: "rename <id> <new-name>", Short: "rename an entry (cascades into masters' dialers)",
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := parseID(args[0])
			if err != nil {
				return err
			}
			return withClient(cmd, func(ctx context.Context, cl emxv1.DaemonClient) error {
				if _, err := cl.EntryRename(ctx, &emxv1.RenameRequest{Id: id, NewName: args[1]}); err != nil {
					return err
				}
				fmt.Fprintln(cmd.OutOrStdout(), "renamed")
				return nil
			})
		},
	}
}

func entryAddCmd() *cobra.Command {
	var link, outbound, dialer string
	c := &cobra.Command{
		Use:   "add [name]",
		Short: "add an outbound from a share link or raw JSON (--dialer makes it a master)",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := ""
			if len(args) == 1 {
				name = args[0]
			}
			if link == "" && outbound == "" {
				return fmt.Errorf("provide --link <share-link> or --outbound <json>")
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), 5*time.Second)
			defer cancel()
			client, conn, err := dialReady(ctx)
			if err != nil {
				return errDaemon(err)
			}
			defer conn.Close()
			reply, err := client.EntryAdd(ctx, &emxv1.EntryAddRequest{
				Name: name, Link: link, OutboundJson: outbound, Dialer: dialer,
			})
			if err != nil {
				return err
			}
			e := reply.Entry
			kind := "entry"
			if e.IsMaster {
				kind = "master"
			}
			fmt.Fprintf(cmd.OutOrStdout(), "added %s %q (id %d)\n", kind, e.Name, e.Id)
			return nil
		},
	}
	c.Flags().StringVar(&link, "link", "", "vless/vmess/trojan/ss share link")
	c.Flags().StringVar(&outbound, "outbound", "", "raw outbound JSON")
	c.Flags().StringVar(&dialer, "dialer", "", "dialer refs (xray:N,xraysub:N,proxy:N) — makes a master")
	return c
}

func entryListCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "ls",
		Short:   "list entries",
		Aliases: []string{"list"},
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx, cancel := context.WithTimeout(cmd.Context(), 3*time.Second)
			defer cancel()
			client, conn, err := dialReady(ctx)
			if err != nil {
				return errDaemon(err)
			}
			defer conn.Close()
			reply, err := client.EntryList(ctx, &emxv1.Empty{})
			if err != nil {
				return err
			}
			tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "ID\tNAME\tKIND\tENABLED\tDIALER")
			for _, e := range reply.Entries {
				kind := "entry"
				if e.IsMaster {
					kind = "master"
				}
				fmt.Fprintf(tw, "%d\t%s\t%s\t%v\t%s\n", e.Id, e.Name, kind, e.Enabled, e.Dialer)
			}
			return tw.Flush()
		},
	}
}

func entryRemoveCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "rm <id>",
		Short:   "remove an entry",
		Aliases: []string{"remove", "del"},
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := strconv.ParseUint(args[0], 10, 32)
			if err != nil {
				return fmt.Errorf("bad id %q", args[0])
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), 5*time.Second)
			defer cancel()
			client, conn, err := dialReady(ctx)
			if err != nil {
				return errDaemon(err)
			}
			defer conn.Close()
			if _, err := client.EntryRemove(ctx, &emxv1.IdRequest{Id: uint32(id)}); err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), "removed")
			return nil
		},
	}
}
