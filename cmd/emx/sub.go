package main

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	emxv1 "github.com/ehsan200/em-xray/api/emxv1"
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
		subRenameCmd(), subInfoCmd(), subSetCmd(), subTestCmd(),
	)
	return c
}

// subSetCmd changes refresh interval / node cap / UA on an existing subscription.
// Unspecified flags keep their current value.
func subSetCmd() *cobra.Command {
	var interval, cap int
	var ua string
	c := &cobra.Command{
		Use: "set <id>", Short: "change a subscription's refresh interval / cap / user-agent",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := parseID(args[0])
			if err != nil {
				return err
			}
			f := cmd.Flags()
			if !f.Changed("interval") && !f.Changed("cap") && !f.Changed("ua") {
				return fmt.Errorf("nothing to change: pass --interval, --cap and/or --ua")
			}
			return withClient(cmd, func(ctx context.Context, cl emxv1.DaemonClient) error {
				// Start from current values so unspecified flags are preserved.
				list, err := cl.SubList(ctx, &emxv1.Empty{})
				if err != nil {
					return err
				}
				var cur *emxv1.SubInfo
				for _, s := range list.Subs {
					if s.Id == id {
						cur = s
						break
					}
				}
				if cur == nil {
					return fmt.Errorf("no subscription with id %d", id)
				}
				req := &emxv1.SubOptionsRequest{
					Id: id, IntervalSec: cur.IntervalSec, NodeCap: cur.NodeCap, UserAgent: cur.UserAgent,
				}
				if f.Changed("interval") {
					req.IntervalSec = int32(interval)
				}
				if f.Changed("cap") {
					req.NodeCap = int32(cap)
				}
				if f.Changed("ua") {
					req.UserAgent = ua
				}
				reply, err := cl.SubSetOptions(ctx, req)
				if err != nil {
					return err
				}
				printSubInfo(cmd, reply.Sub)
				return nil
			})
		},
	}
	c.Flags().IntVar(&interval, "interval", 0, "refresh interval seconds (0 = 12h default)")
	c.Flags().IntVar(&cap, "cap", 0, "max active nodes (0 = 30 default)")
	c.Flags().StringVar(&ua, "ua", "", "User-Agent ('' = v2rayN default)")
	return c
}

// subInfoCmd prints the full metadata for one subscription: quota (up/down/used
// of total), expiry, last-fetch time, UA, refresh interval and node cap.
func subInfoCmd() *cobra.Command {
	return &cobra.Command{
		Use: "info NAME", Short: "show a subscription's metadata (quota, expiry, last fetch)",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return withClient(cmd, func(ctx context.Context, cl emxv1.DaemonClient) error {
				reply, err := cl.SubList(ctx, &emxv1.Empty{})
				if err != nil {
					return err
				}
				for _, s := range reply.Subs {
					if s.Name != args[0] {
						continue
					}
					printSubInfo(cmd, s)
					return nil
				}
				return fmt.Errorf("no subscription named %q", args[0])
			})
		},
	}
}

func printSubInfo(cmd *cobra.Command, s *emxv1.SubInfo) {
	fmt.Fprintln(cmd.OutOrStdout(), renderSubCard(s))
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
				fmt.Fprintln(tw, "ID\tNAME\tENABLED\tNODES\tACTIVE\tUSED\tEXPIRES\tERROR")
				for _, s := range reply.Subs {
					fmt.Fprintf(tw, "%d\t%s\t%v\t%d\t%d\t%s\t%s\t%s\n",
						s.Id, s.Name, s.Enabled, s.NodeCount, s.ActiveCount,
						usedLine(s), expiryShort(s.Expire), s.LastError)
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
					fmt.Fprintf(cmd.OutOrStdout(), "refreshed: %d nodes %s\n", reply.Nodes, changeSummary(reply.Added, reply.Removed))
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
				fmt.Fprintln(tw, "FINGERPRINT\tNAME\tACTIVE\tDISABLED\tLATENCY\tSTATE")
				for _, n := range reply.Nodes {
					lat := "-"
					if n.LatencyMs > 0 {
						lat = strconv.Itoa(int(n.LatencyMs)) + "ms"
					}
					state := parkedLabel(n.ParkedUntil)
					if n.Rejected != "" {
						state = "REFUSED by xray"
					}
					fmt.Fprintf(tw, "%s\t%s\t%v\t%v\t%s\t%s\n", n.Fingerprint, n.Name, n.Active, n.Disabled, lat, state)
				}
				if err := tw.Flush(); err != nil {
					return err
				}
				for _, n := range reply.Nodes {
					if n.Rejected != "" {
						fmt.Fprintf(cmd.OutOrStdout(), "\n%s (%s) refused by xray:\n  %s\n", n.Name, n.Fingerprint, n.Rejected)
					}
				}
				return nil
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

// ---- subscription metadata formatting -------------------------------------

// humanBytes renders a byte count as a compact human string (e.g. 1.5 GB).
func humanBytes(n int64) string {
	if n <= 0 {
		return "0 B"
	}
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for x := n / unit; x >= unit; x /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTPE"[exp])
}

// usedLine is the compact used/total for the ls table.
func usedLine(s *emxv1.SubInfo) string {
	used := s.Upload + s.Download
	if s.Total > 0 {
		return fmt.Sprintf("%s/%s", humanBytes(used), humanBytes(s.Total))
	}
	if used > 0 {
		return humanBytes(used)
	}
	return "-"
}

func expiryLine(expire int64) string {
	if expire <= 0 {
		return "never"
	}
	t := time.Unix(expire, 0)
	d := time.Until(t)
	if d <= 0 {
		return fmt.Sprintf("%s (EXPIRED)", t.Format("2006-01-02"))
	}
	return fmt.Sprintf("%s (in %s)", t.Format("2006-01-02"), roundDays(d))
}

func expiryShort(expire int64) string {
	if expire <= 0 {
		return "-"
	}
	t := time.Unix(expire, 0)
	if time.Until(t) <= 0 {
		return "EXPIRED"
	}
	return t.Format("2006-01-02")
}

func fetchLine(last int64) string {
	if last <= 0 {
		return "never"
	}
	t := time.Unix(last, 0)
	return fmt.Sprintf("%s (%s ago)", t.Format("2006-01-02 15:04"), roundDur(time.Since(t)))
}

func intervalLine(sec int32) string {
	if sec <= 0 {
		return "12h (default)"
	}
	return compactDur(time.Duration(sec) * time.Second)
}

// compactDur formats a duration on whole-unit boundaries (12h, 30m) and falls
// back to the standard form otherwise.
func compactDur(d time.Duration) string {
	switch {
	case d >= time.Hour && d%time.Hour == 0:
		return fmt.Sprintf("%dh", d/time.Hour)
	case d >= time.Minute && d%time.Minute == 0:
		return fmt.Sprintf("%dm", d/time.Minute)
	default:
		return d.String()
	}
}

func capLine(cap int32) string {
	if cap <= 0 {
		return "30 (default)"
	}
	return strconv.Itoa(int(cap))
}

// subMetaText renders a subscription's metadata card (for the TUI, which prints
// it above the menu).
func subMetaText(s *emxv1.SubInfo) string {
	return renderSubCard(s)
}

// changeSummary describes a refresh's node diff: "(no change)" or "(+A -R)".
func changeSummary(added, removed int32) string {
	if added == 0 && removed == 0 {
		return cardLabel.Render("(no change)")
	}
	parts := []string{}
	if added > 0 {
		parts = append(parts, goodStyle.Render(fmt.Sprintf("+%d", added)))
	}
	if removed > 0 {
		parts = append(parts, badStyle.Render(fmt.Sprintf("-%d", removed)))
	}
	return "(" + strings.Join(parts, " ") + " updated)"
}

// daysLeft returns whole days until the given unix expiry (may be negative).
func daysLeft(expire int64) int {
	return int(time.Until(time.Unix(expire, 0)).Hours() / 24)
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// roundDays renders a positive duration coarsely (days when ≥1d, else h/m).
func roundDays(d time.Duration) string {
	if d >= 24*time.Hour {
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
	return roundDur(d)
}

func roundDur(d time.Duration) string {
	if d >= time.Hour {
		return d.Round(time.Minute).String()
	}
	return d.Round(time.Second).String()
}

// parkedLabel renders a node's park state: "-" when in its pool, else when
// the dead node is put back on trial.
func parkedLabel(until int64) string {
	if until == 0 {
		return "-"
	}
	return "dead, retry " + time.Unix(until, 0).Format("15:04")
}
