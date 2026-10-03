package main

import (
	"context"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
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
		entryDuplicateCmd(), entryEditCmd(), entryTestCmd(), entryMuxCmd(), entryDialerCmd())
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

func entryDialerCmd() *cobra.Command {
	var add, rm []string
	var clear bool
	c := &cobra.Command{
		Use:   "dialer <id|ids|masters|all> [refs...]",
		Short: "show or change masters' dialers (several subs/entries merge into one pool)",
		Long: "Without refs or flags, prints the dialer refs. Refs replace the dialer;\n" +
			"--add/--rm edit it in place; --clear turns a master back into a plain\n" +
			"entry. Every ref is xraysub:NAME (a subscription's nodes) or xray:NAME\n" +
			"(one entry). All refs feed one pool the master's balancer picks from, so\n" +
			"a dead subscription doesn't take the master down.\n\n" +
			"Select several entries with a comma list of ids, `masters` (every entry\n" +
			"with a dialer) or `all`: they're validated together (nothing changes if\n" +
			"any is refused) and applied at once. --rm skips entries without the ref.\n\n" +
			"  emx entry dialer 3 xraysub:mysub xraysub:backup\n" +
			"  emx entry dialer 3 --add xray:my-vps\n" +
			"  emx entry dialer 3,5,7 --add xraysub:backup\n" +
			"  emx entry dialer masters --rm xraysub:mysub",
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return withClient(cmd, func(ctx context.Context, cl emxv1.DaemonClient) error {
				list, err := cl.EntryList(ctx, &emxv1.Empty{})
				if err != nil {
					return err
				}
				sel, err := selectEntries(list.Entries, args[0])
				if err != nil {
					return err
				}
				out := cmd.OutOrStdout()
				if len(args) == 1 && len(add) == 0 && len(rm) == 0 && !clear {
					for _, e := range sel {
						printDialer(out, e, len(sel) > 1)
					}
					return nil
				}
				items, err := editDialers(sel, args[1:], add, rm, clear)
				if err != nil {
					return err
				}
				reply, err := cl.EntryBulkDialer(ctx, &emxv1.EntryBulkDialerRequest{Items: items})
				if err != nil {
					return err
				}
				if len(reply.Entries) == 0 {
					fmt.Fprintln(out, "no change")
				}
				for _, e := range reply.Entries {
					printDialer(out, e, true)
				}
				return nil
			})
		},
	}
	c.Flags().StringArrayVar(&add, "add", nil, "add a ref (xraysub:NAME or xray:NAME); repeatable")
	c.Flags().StringArrayVar(&rm, "rm", nil, "remove a ref; repeatable")
	c.Flags().BoolVar(&clear, "clear", false, "remove every ref (the entry stops being a master)")
	return c
}

// printDialer prints an entry's dialer: one ref per line, or on one line
// prefixed by the entry name when listing several.
func printDialer(out io.Writer, e *emxv1.EntryInfo, named bool) {
	switch {
	case e.Dialer == "":
		fmt.Fprintf(out, "%s: no dialer (plain entry)\n", e.Name)
	case named:
		fmt.Fprintf(out, "%s: %s\n", e.Name, e.Dialer)
	default:
		for _, r := range splitRefs(e.Dialer) {
			fmt.Fprintln(out, r)
		}
	}
}

// selectEntries resolves an entry selector: an id, a comma list of ids,
// "masters" (entries with a dialer) or "all".
func selectEntries(entries []*emxv1.EntryInfo, sel string) ([]*emxv1.EntryInfo, error) {
	var out []*emxv1.EntryInfo
	switch sel {
	case "all":
		out = entries
	case "masters":
		for _, e := range entries {
			if e.IsMaster {
				out = append(out, e)
			}
		}
	default:
		byID := map[uint32]*emxv1.EntryInfo{}
		for _, e := range entries {
			byID[e.Id] = e
		}
		for _, part := range splitRefs(sel) {
			id, err := parseID(part)
			if err != nil {
				return nil, err
			}
			e, ok := byID[id]
			if !ok {
				return nil, fmt.Errorf("no entry with id %d", id)
			}
			if !slices.Contains(out, e) {
				out = append(out, e)
			}
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no entries match %q", sel)
	}
	return out, nil
}

// editDialers applies one edit (see editDialer) to each selected entry. A
// removed ref is skipped on entries that don't have it, but must be on at
// least one, so a typo doesn't pass silently.
func editDialers(sel []*emxv1.EntryInfo, set, add, rm []string, clear bool) ([]*emxv1.EntryDialerRequest, error) {
	rmRefs := splitRefs(strings.Join(rm, ","))
	for _, r := range rmRefs {
		if !slices.ContainsFunc(sel, func(e *emxv1.EntryInfo) bool { return slices.Contains(splitRefs(e.Dialer), r) }) {
			return nil, fmt.Errorf("no selected entry has dialer ref %q", r)
		}
	}
	var items []*emxv1.EntryDialerRequest
	for _, e := range sel {
		have := splitRefs(e.Dialer)
		var own []string
		for _, r := range rmRefs {
			if slices.Contains(have, r) {
				own = append(own, r)
			}
		}
		d, err := editDialer(e.Dialer, set, add, own, clear)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", e.Name, err)
		}
		items = append(items, &emxv1.EntryDialerRequest{Id: e.Id, Dialer: d})
	}
	return items, nil
}

// splitRefs splits a comma-separated dialer into trimmed, non-empty refs.
func splitRefs(dialer string) []string {
	var out []string
	for _, r := range strings.Split(dialer, ",") {
		if r = strings.TrimSpace(r); r != "" {
			out = append(out, r)
		}
	}
	return out
}

// editDialer computes a new dialer from the current one: set (if non-empty)
// replaces it, clear empties it, then rm drops and add appends refs. Each
// argument may itself be a comma list. Duplicates are dropped; removing a ref
// that isn't there is an error so a typo doesn't pass silently.
func editDialer(cur string, set, add, rm []string, clear bool) (string, error) {
	if clear && len(set) > 0 {
		return "", fmt.Errorf("--clear and refs are exclusive")
	}
	refs := splitRefs(cur)
	if clear {
		refs = nil
	}
	if len(set) > 0 {
		refs = splitRefs(strings.Join(set, ","))
	}
	for _, r := range splitRefs(strings.Join(rm, ",")) {
		i := slices.Index(refs, r)
		if i < 0 {
			return "", fmt.Errorf("dialer has no ref %q (have: %s)", r, strings.Join(refs, ", "))
		}
		refs = append(refs[:i], refs[i+1:]...)
	}
	for _, r := range splitRefs(strings.Join(add, ",")) {
		if slices.Index(refs, r) < 0 {
			refs = append(refs, r)
		}
	}
	var out []string
	for _, r := range refs {
		if slices.Index(out, r) < 0 {
			out = append(out, r)
		}
	}
	return strings.Join(out, ","), nil
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
