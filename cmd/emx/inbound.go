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

	emxv1 "github.com/gravisun/em-xray/api/emxv1"
	"github.com/spf13/cobra"
)

func inboundCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "in",
		Short: "manage server inbounds (vless/vmess/socks listeners)",
		RunE:  func(cmd *cobra.Command, _ []string) error { return runMenu(cmd, "in") },
	}
	c.AddCommand(inboundAddCmd(), inboundListCmd(), inboundRemoveCmd())
	return c
}

func inboundAddCmd() *cobra.Command {
	var template, target, host string
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

			ctx, cancel := context.WithTimeout(cmd.Context(), 5*time.Second)
			defer cancel()
			client, conn, err := dialReady(ctx)
			if err != nil {
				return errDaemon(err)
			}
			defer conn.Close()
			reply, err := client.InboundAdd(ctx, &emxv1.InboundAddRequest{
				Name: name, Template: template, Target: target, PublicHost: host, Port: int32(port),
			})
			if err != nil {
				return err
			}
			printInbound(cmd.OutOrStdout(), reply.Inbound, true)
			return nil
		},
	}
	c.Flags().StringVarP(&template, "template", "t", "", "template (default vless-reality; see `emx template ls`)")
	c.Flags().StringVar(&target, "to", "", "egress target: master:NAME | xray:NAME | direct")
	c.Flags().StringVar(&host, "host", "", "public host/IP clients dial (for the share link)")
	c.Flags().IntVar(&port, "port", 0, "listen port (0 = auto-assign)")
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
			fmt.Fprintln(tw, "ID\tNAME\tPROTO\tPORT\tSECURITY\tTARGET\tENABLED")
			for _, in := range reply.Inbounds {
				fmt.Fprintf(tw, "%d\t%s\t%s\t%d\t%s\t%s\t%v\n", in.Id, in.Name, in.Protocol, in.Port, in.Security, in.Target, in.Enabled)
			}
			tw.Flush()
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
			if _, err := client.InboundRemove(ctx, &emxv1.IdRequest{Id: uint32(id)}); err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), "removed")
			return nil
		},
	}
}

func printInbound(w interface{ Write([]byte) (int, error) }, in *emxv1.InboundInfo, withLink bool) {
	fmt.Fprintf(w, "added inbound %q (id %d): %s on :%d → %s\n", in.Name, in.Id, in.Protocol, in.Port, in.Target)
	if withLink && in.ShareLink != "" {
		fmt.Fprintf(w, "\nclient link:\n  %s\n", in.ShareLink)
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
