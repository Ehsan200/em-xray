package main

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"text/tabwriter"

	emxv1 "github.com/ehsan200/em-xray/api/emxv1"
	"github.com/spf13/cobra"
)

// inboundUserCmd groups per-inbound multi-user management under `emx in user`.
func inboundUserCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "user",
		Short: "manage extra clients on an inbound (multi-user + byte caps)",
	}
	c.AddCommand(userAddCmd(), userListCmd(), userRemoveCmd(),
		userEnableCmd(true), userEnableCmd(false), userQRCmd())
	return c
}

func userAddCmd() *cobra.Command {
	var cap string
	c := &cobra.Command{
		Use: "add <inbound-id> <name>", Short: "add a client (own uuid/link); --cap sets a byte quota",
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := parseID(args[0])
			if err != nil {
				return err
			}
			capBytes, err := parseSize(cap)
			if err != nil {
				return err
			}
			return withClient(cmd, func(ctx context.Context, cl emxv1.DaemonClient) error {
				reply, err := cl.InboundUserAdd(ctx, &emxv1.InboundUserAddRequest{
					InboundId: id, Name: args[1], ByteCap: capBytes,
				})
				if err != nil {
					return err
				}
				u := reply.User
				fmt.Fprintf(cmd.OutOrStdout(), "added user %q (id %d)%s\n\nclient link:\n  %s\n",
					u.Name, u.Id, capLabel(u.ByteCap), u.ShareLink)
				return nil
			})
		},
	}
	c.Flags().StringVar(&cap, "cap", "", "byte quota, e.g. 10GB, 500MB (blank = unlimited)")
	return c
}

func userListCmd() *cobra.Command {
	return &cobra.Command{
		Use: "ls <inbound-id>", Short: "list an inbound's users with usage vs cap", Aliases: []string{"list"},
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := parseID(args[0])
			if err != nil {
				return err
			}
			return withClient(cmd, func(ctx context.Context, cl emxv1.DaemonClient) error {
				reply, err := cl.InboundUserList(ctx, &emxv1.IdRequest{Id: id})
				if err != nil {
					return err
				}
				if len(reply.Users) == 0 {
					fmt.Fprintln(cmd.OutOrStdout(), "no extra users (the inbound's own link is the primary client)")
					return nil
				}
				tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
				fmt.Fprintln(tw, "ID\tNAME\tENABLED\tUSED\tCAP\tSTATE")
				for _, u := range reply.Users {
					state := "ok"
					if u.OverCap {
						state = "OVER CAP"
					} else if !u.Enabled {
						state = "disabled"
					}
					fmt.Fprintf(tw, "%d\t%s\t%v\t%s\t%s\t%s\n",
						u.Id, u.Name, u.Enabled, humanBytes(u.UsedUp+u.UsedDown), capValue(u.ByteCap), state)
				}
				return tw.Flush()
			})
		},
	}
}

func userRemoveCmd() *cobra.Command {
	return &cobra.Command{
		Use: "rm <user-id>", Short: "remove a user", Aliases: []string{"remove", "del"},
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := parseID(args[0])
			if err != nil {
				return err
			}
			return withClient(cmd, func(ctx context.Context, cl emxv1.DaemonClient) error {
				if _, err := cl.InboundUserRemove(ctx, &emxv1.IdRequest{Id: id}); err != nil {
					return err
				}
				fmt.Fprintln(cmd.OutOrStdout(), "removed")
				return nil
			})
		},
	}
}

func userEnableCmd(enable bool) *cobra.Command {
	use, verb := "enable <user-id>", "enabled"
	if !enable {
		use, verb = "disable <user-id>", "disabled"
	}
	return &cobra.Command{
		Use: use, Short: verb[:len(verb)-1] + " a user", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := parseID(args[0])
			if err != nil {
				return err
			}
			return withClient(cmd, func(ctx context.Context, cl emxv1.DaemonClient) error {
				if _, err := cl.InboundUserSetEnabled(ctx, &emxv1.SetEnabledRequest{Id: id, Enabled: enable}); err != nil {
					return err
				}
				fmt.Fprintln(cmd.OutOrStdout(), verb)
				return nil
			})
		},
	}
}

func userQRCmd() *cobra.Command {
	return &cobra.Command{
		Use: "qr <inbound-id> <user-name>", Short: "print a user's client link as a QR code",
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := parseID(args[0])
			if err != nil {
				return err
			}
			return withClient(cmd, func(ctx context.Context, cl emxv1.DaemonClient) error {
				reply, err := cl.InboundUserList(ctx, &emxv1.IdRequest{Id: id})
				if err != nil {
					return err
				}
				for _, u := range reply.Users {
					if u.Name != args[1] {
						continue
					}
					fmt.Fprintf(cmd.OutOrStdout(), "%s\n\n%s\n%s\n", u.Name, renderQR(u.ShareLink), u.ShareLink)
					return nil
				}
				return fmt.Errorf("no user named %q on inbound %d", args[1], id)
			})
		},
	}
}

func capLabel(cap int64) string {
	if cap <= 0 {
		return ""
	}
	return " · cap " + humanBytes(cap)
}

func capValue(cap int64) string {
	if cap <= 0 {
		return "∞"
	}
	return humanBytes(cap)
}

// parseSize parses a byte size: a bare number, or a value with a KB/MB/GB/TB
// suffix (binary, 1024-based). Empty or "0" means 0 (unlimited).
func parseSize(s string) (int64, error) {
	s = strings.TrimSpace(strings.ToUpper(s))
	if s == "" || s == "0" {
		return 0, nil
	}
	units := []struct {
		suffix string
		mult   int64
	}{
		{"TB", 1 << 40}, {"T", 1 << 40},
		{"GB", 1 << 30}, {"G", 1 << 30},
		{"MB", 1 << 20}, {"M", 1 << 20},
		{"KB", 1 << 10}, {"K", 1 << 10},
		{"B", 1},
	}
	for _, u := range units {
		if strings.HasSuffix(s, u.suffix) {
			num := strings.TrimSpace(strings.TrimSuffix(s, u.suffix))
			f, err := strconv.ParseFloat(num, 64)
			if err != nil || f < 0 {
				return 0, fmt.Errorf("bad size %q", s)
			}
			return int64(f * float64(u.mult)), nil
		}
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("bad size %q (try 10GB, 500MB)", s)
	}
	return n, nil
}
