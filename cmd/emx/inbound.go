package main

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	emxv1 "github.com/ehsan200/em-xray/api/emxv1"
	"github.com/spf13/cobra"
)

func inboundCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "in",
		Short: "manage server inbounds (vless/vmess/socks listeners)",
		RunE:  func(cmd *cobra.Command, _ []string) error { return runMenu(cmd, "in") },
	}
	c.AddCommand(inboundAddCmd(), inboundListCmd(), inboundRemoveCmd(),
		inboundDuplicateCmd(), inboundEditCmd(), inboundQRCmd(), inboundUserCmd())
	return c
}

func inboundDuplicateCmd() *cobra.Command {
	return &cobra.Command{
		Use: "duplicate <id> [new-name]", Short: "clone an inbound (fresh keys + endpoint)",
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
				reply, err := cl.InboundDuplicate(ctx, &emxv1.DuplicateRequest{Id: id, NewName: name})
				if err != nil {
					return err
				}
				printInbound(cmd.OutOrStdout(), reply.Inbound, true)
				if isCaddyInboundInfo(reply.Inbound) {
					return maybeApplyCaddy(cmd)
				}
				return nil
			})
		},
	}
}

func inboundEditCmd() *cobra.Command {
	return &cobra.Command{
		Use: "edit <id>", Short: "edit an inbound's JSON in $EDITOR (blank keys regenerate on save)",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := parseID(args[0])
			if err != nil {
				return err
			}
			return withClient(cmd, func(ctx context.Context, cl emxv1.DaemonClient) error {
				cur, err := cl.InboundGetConfig(ctx, &emxv1.IdRequest{Id: id})
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
				wasCaddy := isCaddyInboundJSON(cur.Json)
				reply, err := cl.InboundSetConfig(ctx, &emxv1.SetConfigRequest{Id: id, Json: edited})
				if err != nil {
					return err
				}
				printInboundVerb(cmd.OutOrStdout(), "updated", reply.Inbound, true)
				if wasCaddy || isCaddyInboundInfo(reply.Inbound) {
					return maybeApplyCaddy(cmd)
				}
				return nil
			})
		},
	}
}

func inboundAddCmd() *cobra.Command {
	var template, target, host, domain, path, email, xhttpMode string
	var port int
	c := &cobra.Command{
		Use:   "add [name]",
		Short: "create a server inbound (auto-generates keys, prints the client link)",
		Long: "Create a server listener. Everything is auto-generated — run `emx in add myserver`\n" +
			"for an instant vless-reality server, or `emx in add` for a step-by-step wizard.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := ""
			if len(args) == 1 {
				name = args[0]
			}
			// Wizard: fill blanks interactively when a name wasn't given and we
			// have a terminal. With a name + flags, run non-interactively.
			if name == "" {
				in := bufio.NewReader(os.Stdin)
				out := cmd.OutOrStdout()
				name = ask(in, out, "name", "")
				if name == "" {
					return fmt.Errorf("name is required")
				}
				template = ask(in, out, "template", firstNonEmpty(template, "vless-reality"))
				target = ask(in, out, "target (master:NAME | xray:NAME | direct)", firstNonEmpty(target, "direct"))
				host = ask(in, out, "public host/IP for the client link (optional)", host)
			}
			if target == "" {
				target = "direct"
			}
			if domain != "" {
				if host != "" && host != domain {
					return fmt.Errorf("--host and --domain disagree")
				}
				host = domain
			}

			ctx, cancel := context.WithTimeout(cmd.Context(), 5*time.Second)
			defer cancel()
			client, conn, err := dialReady(ctx)
			if err != nil {
				return errDaemon(err)
			}
			defer conn.Close()
			reply, err := client.InboundAdd(ctx, &emxv1.InboundAddRequest{
				Name: name, Template: template, Target: target, PublicHost: host, Port: int32(port),
				Path: path, Email: email, XhttpMode: xhttpMode,
			})
			if err != nil {
				return err
			}
			printInbound(cmd.OutOrStdout(), reply.Inbound, true)
			if isCaddyInboundInfo(reply.Inbound) {
				return maybeApplyCaddy(cmd)
			}
			return nil
		},
	}
	c.Flags().StringVarP(&template, "template", "t", "", "template name (default vless-reality; see: emx template ls)")
	c.Flags().StringVar(&target, "to", "", "egress target: master:NAME | xray:NAME | direct")
	c.Flags().StringVar(&host, "host", "", "public host/IP clients dial (for the share link)")
	c.Flags().StringVar(&domain, "domain", "", "public domain for Caddy and the client link")
	c.Flags().IntVar(&port, "port", 0, "listen port (0 = auto-assign)")
	c.Flags().StringVar(&path, "path", "", "transport path (Caddy/XHTTP: blank generates a random path)")
	c.Flags().StringVar(&email, "email", "", "primary client's xray stats email")
	c.Flags().StringVar(&xhttpMode, "xhttp-mode", "", "XHTTP mode: auto | packet-up | stream-up | stream-one")
	return c
}

func inboundListCmd() *cobra.Command {
	var links bool
	c := &cobra.Command{
		Use:     "ls",
		Short:   "list inbounds (with client share links)",
		Aliases: []string{"list"},
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx, cancel := context.WithTimeout(cmd.Context(), 3*time.Second)
			defer cancel()
			client, conn, err := dialReady(ctx)
			if err != nil {
				return errDaemon(err)
			}
			defer conn.Close()
			reply, err := client.InboundList(ctx, &emxv1.Empty{})
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "ID\tNAME\tPROTO\tENDPOINT\tSECURITY\tTARGET\tENABLED")
			direct := 0
			for _, in := range reply.Inbounds {
				// A direct target is the one egress that reveals this server's own
				// IP. Mark it in the listing so it is never a silent surprise.
				target := in.Target
				if isDirectTarget(target) {
					target += "  ⚠ this server's IP"
					if in.Enabled {
						direct++
					}
				}
				endpoint := fmt.Sprintf(":%d", in.Port)
				if in.Listen != "" && in.Port == 0 {
					endpoint = in.Listen
				}
				fmt.Fprintf(tw, "%d\t%s\t%s\t%s\t%s\t%s\t%v\n", in.Id, in.Name, in.Protocol, endpoint, in.Security, target, in.Enabled)
			}
			tw.Flush()
			if direct > 0 {
				fmt.Fprintf(out, "\n⚠ %d enabled inbound(s) egress from this server's own IP (target: direct).\n", direct)
			}
			if links {
				fmt.Fprintln(out)
				for _, in := range reply.Inbounds {
					if in.ShareLink != "" {
						fmt.Fprintf(out, "%s:\n  %s\n", in.Name, in.ShareLink)
					}
				}
			}
			return nil
		},
	}
	c.Flags().BoolVar(&links, "links", false, "also print client share links")
	return c
}

// isDirectTarget reports whether an inbound's target egresses straight off this
// box. Blank is the legacy spelling of "direct" and means the same thing.
func isDirectTarget(target string) bool {
	t := strings.TrimSpace(target)
	return t == "" || t == "direct"
}

func inboundRemoveCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "rm <id>",
		Short:   "remove an inbound",
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
			managed := false
			if list, listErr := client.InboundList(ctx, &emxv1.Empty{}); listErr == nil {
				for _, in := range list.Inbounds {
					if in.Id == uint32(id) {
						managed = isCaddyInboundInfo(in)
						break
					}
				}
			}
			if _, err := client.InboundRemove(ctx, &emxv1.IdRequest{Id: uint32(id)}); err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), "removed")
			if managed {
				return maybeApplyCaddy(cmd)
			}
			return nil
		},
	}
}

func printInbound(w interface{ Write([]byte) (int, error) }, in *emxv1.InboundInfo, withLink bool) {
	printInboundVerb(w, "added", in, withLink)
}

func printInboundVerb(w interface{ Write([]byte) (int, error) }, verb string, in *emxv1.InboundInfo, withLink bool) {
	endpoint := fmt.Sprintf(":%d", in.Port)
	if in.Listen != "" && in.Port == 0 {
		endpoint = in.Listen
	}
	fmt.Fprintf(w, "%s inbound %q (id %d): %s on %s → %s\n", verb, in.Name, in.Id, in.Protocol, endpoint, in.Target)
	if withLink && in.ShareLink != "" {
		fmt.Fprintf(w, "\nclient link:\n  %s\n", in.ShareLink)
		if in.TgLink != "" {
			fmt.Fprintf(w, "\nTelegram proxy link:\n  %s\n", in.TgLink)
		}
		if strings.Contains(in.ShareLink, "SERVER_IP") {
			fmt.Fprintln(w, "\n(note: set --host <public-ip> to embed your real address in the link)")
		}
	}
}

// ask prints "label [def]: " and returns the trimmed input, or def if empty.
func ask(r *bufio.Reader, w interface{ Write([]byte) (int, error) }, label, def string) string {
	if def != "" {
		fmt.Fprintf(w, "%s [%s]: ", label, def)
	} else {
		fmt.Fprintf(w, "%s: ", label)
	}
	line, _ := r.ReadString('\n')
	line = strings.TrimSpace(line)
	if line == "" {
		return def
	}
	return line
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
