package main

import (
	"context"
	"fmt"
	"strconv"
	"text/tabwriter"
	"time"

	emxv1 "github.com/ehsan200/em-xray/api/emxv1"
	"github.com/spf13/cobra"
)

func entryCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "entry",
		Short: "manage outbounds (and masters)",
		RunE:  func(cmd *cobra.Command, _ []string) error { return runMenu(cmd, "entry") },
	}
	c.AddCommand(entryAddCmd(), entryListCmd(), entryRemoveCmd(), entryRenameCmd(),
		entryDuplicateCmd(), entryEditCmd(), entryTestCmd(), entryMuxCmd())
	return c
}

func entryDuplicateCmd() *cobra.Command {
	return &cobra.Command{
		Use: "duplicate <id> [new-name]", Short: "clone an entry (same outbound + dialer)",
		Aliases: []string{"dup", "copy"}, Args: cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := parseID(args[0])
			if err != nil {
				return err
			}
			name := ""
			if len(args) == 2 {
				name = args[1]
			}
			return withClient(cmd, func(ctx context.Context, cl emxv1.DaemonClient) error {
				reply, err := cl.EntryDuplicate(ctx, &emxv1.DuplicateRequest{Id: id, NewName: name})
				if err != nil {
					return err
				}
				fmt.Fprintf(cmd.OutOrStdout(), "duplicated to %q (id %d)\n", reply.Entry.Name, reply.Entry.Id)
				return nil
			})
		},
	}
}

func entryEditCmd() *cobra.Command {
	return &cobra.Command{
		Use: "edit <id>", Short: "edit an entry's outbound JSON in $EDITOR",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := parseID(args[0])
			if err != nil {
				return err
			}
			return withClient(cmd, func(ctx context.Context, cl emxv1.DaemonClient) error {
				cur, err := cl.EntryGetConfig(ctx, &emxv1.IdRequest{Id: id})
				if err != nil {
					return err
				}
				edited, changed, err := editInEditor(cur.Json, ".json")
				if err != nil {
					return err
				}
				if !changed {
					fmt.Fprintln(cmd.OutOrStdout(), "no changes")
					return nil
				}
				reply, err := cl.EntrySetConfig(ctx, &emxv1.SetConfigRequest{Id: id, Json: edited})
				if err != nil {
					return err
				}
				fmt.Fprintf(cmd.OutOrStdout(), "updated %q (id %d)\n", reply.Entry.Name, reply.Entry.Id)
				return nil
			})
		},
	}
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
	var mux bool
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
				Name: name, Link: link, OutboundJson: outbound, Dialer: dialer, Mux: mux,
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
			if mux && e.MuxNote != "" {
				fmt.Fprintf(cmd.OutOrStdout(), "mux stored but inactive: %s\n", e.MuxNote)
			}
			return nil
		},
	}
	c.Flags().StringVar(&link, "link", "", "vless/vmess/trojan/ss/hysteria2 share link")
	c.Flags().StringVar(&outbound, "outbound", "", "raw outbound JSON")
	c.Flags().StringVar(&dialer, "dialer", "", "dialer refs (xray:N,xraysub:N,proxy:N) — makes a master")
	c.Flags().BoolVar(&mux, "mux", false, "multiplex connections over a few tunnels (see `emx entry mux`)")
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
			fmt.Fprintln(tw, "ID\tNAME\tKIND\tENABLED\tMUX\tDIALER")
			for _, e := range reply.Entries {
				kind := "entry"
				if e.IsMaster {
					kind = "master"
				}
				fmt.Fprintf(tw, "%d\t%s\t%s\t%v\t%s\t%s\n", e.Id, e.Name, kind, e.Enabled, muxLabel(e), e.Dialer)
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

func entryMuxCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "mux <id> on|off",
		Short: "multiplex connections through an entry over a few long-lived tunnels",
		Long: "With mux, new connections through the entry start as streams inside an\n" +
			"existing tunnel — no fresh TCP/TLS/WebSocket handshake — at the cost of\n" +
			"shared fate: a broken tunnel drops every stream on it. Applies to VMess,\n" +
			"VLESS (without an XTLS flow) and Trojan over non-multiplexing transports;\n" +
			"`emx entry ls` shows why it doesn't apply to an entry.",
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := parseID(args[0])
			if err != nil {
				return err
			}
			var on bool
			switch args[1] {
			case "on", "true", "1":
				on = true
			case "off", "false", "0":
			default:
				return fmt.Errorf("want on or off, got %q", args[1])
			}
			return withClient(cmd, func(ctx context.Context, cl emxv1.DaemonClient) error {
				reply, err := cl.EntrySetMux(ctx, &emxv1.SetEnabledRequest{Id: id, Enabled: on})
				if err != nil {
					return err
				}
				fmt.Fprintf(cmd.OutOrStdout(), "%s: mux %s\n", reply.Entry.Name, muxLabel(reply.Entry))
				return nil
			})
		},
	}
}

// muxLabel renders an entry's mux state: on/off, or why it can't apply.
func muxLabel(e *emxv1.EntryInfo) string {
	switch {
	case e.MuxNote != "" && e.Mux:
		return "on (inactive: " + e.MuxNote + ")"
	case e.MuxNote != "":
		return "n/a"
	case e.Mux:
		return "on"
	default:
		return "off"
	}
}
