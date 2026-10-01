package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
	emxv1 "github.com/ehsan200/em-xray/api/emxv1"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

// Health timeline states, as sent in PoolHealthNode.states.
const (
	hNone byte = iota
	hUnknown
	hAlive
	hDead
	hParked
	hUplink
	hMixed // render-only: a column merging alive and dead rounds
)

var (
	hAliveStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("42"))
	hDeadStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("196"))
	hMixedStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("214"))
	hMutedStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("241"))
	hUplinkStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("63"))
)

func healthCmd() *cobra.Command {
	var window string
	c := &cobra.Command{
		Use:   "health [master|sub|entry]",
		Short: "show each pool's per-node health timeline (flaps, uptime, pick losses)",
		Long: "Draws the last 30 minutes of pool health, one strip per node: alive, dead,\n" +
			"mixed (both within one column), parked, unknown, uplink down. Filter by a\n" +
			"master's name or by a subscription/entry the pool draws from.",
		Aliases: []string{"pool", "pools"},
		Args:    cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			win, err := time.ParseDuration(window)
			if err != nil {
				return fmt.Errorf("bad --window %q: %w", window, err)
			}
			filter := ""
			if len(args) == 1 {
				filter = args[0]
			}
			return printHealth(cmd, win, func(p *emxv1.PoolHealthPool) bool { return poolMatches(p, filter) })
		},
	}
	c.Flags().StringVar(&window, "window", "30m", "how far back to show (max 30m)")
	return c
}

func subHealthCmd() *cobra.Command {
	var window string
	c := &cobra.Command{
		Use: "health NAME", Short: "show the health timeline of the pools using a subscription",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			win, err := time.ParseDuration(window)
			if err != nil {
				return fmt.Errorf("bad --window %q: %w", window, err)
			}
			return printHealth(cmd, win, func(p *emxv1.PoolHealthPool) bool { return poolUsesSub(p, args[0]) })
		},
	}
	c.Flags().StringVar(&window, "window", "30m", "how far back to show (max 30m)")
	return c
}

func printHealth(cmd *cobra.Command, win time.Duration, keep func(*emxv1.PoolHealthPool) bool) error {
	return withClient(cmd, func(ctx context.Context, cl emxv1.DaemonClient) error {
		reply, err := cl.PoolHealth(ctx, &emxv1.PoolHealthRequest{WindowSec: int64(win.Seconds())})
		if err != nil {
			return err
		}
		fmt.Fprint(cmd.OutOrStdout(), renderHealth(filterPools(reply.Pools, keep), termWidth()))
		return nil
	})
}

func filterPools(pools []*emxv1.PoolHealthPool, keep func(*emxv1.PoolHealthPool) bool) []*emxv1.PoolHealthPool {
	var out []*emxv1.PoolHealthPool
	for _, p := range pools {
		if keep(p) {
			out = append(out, p)
		}
	}
	return out
}

// poolMatches reports whether a pool is served to master `name` or draws from
// a subscription/entry called `name`. Empty matches everything.
func poolMatches(p *emxv1.PoolHealthPool, name string) bool {
	if name == "" {
		return true
	}
	for _, m := range p.Masters {
		if m == name {
			return true
		}
	}
	for _, ref := range strings.Split(p.Refs, ",") {
		if _, n, ok := strings.Cut(ref, ":"); ok && n == name {
			return true
		}
	}
	return false
}

func poolUsesSub(p *emxv1.PoolHealthPool, sub string) bool {
	for _, ref := range strings.Split(p.Refs, ",") {
		if ref == "xraysub:"+sub {
			return true
		}
	}
	return false
}

func termWidth() int {
	if w, _, err := term.GetSize(int(os.Stdout.Fd())); err == nil && w > 40 {
		return w
	}
	return 100
}

// renderHealth draws every pool: a header with its stats, then one row per
// node — role marker, name, timeline strip, uptime, flips, average RTT.
func renderHealth(pools []*emxv1.PoolHealthPool, width int) string {
	if len(pools) == 0 {
		return "no pool health yet (no masters running, or the first poll hasn't happened)\n"
	}
	const nameW = 22
	// marker(2) + name + gap(2) + strip + gap(2) + "100%"(4) + "  99 flips"(10) + "  9999ms"(8)
	stripW := width - 2 - nameW - 2 - 2 - 4 - 10 - 8
	if stripW < 10 {
		stripW = 10
	}
	var b strings.Builder
	for i, p := range pools {
		if i > 0 {
			b.WriteString("\n")
		}
		span := time.Duration(p.Rounds) * time.Duration(p.PollSec) * time.Second
		alive := 0
		for _, n := range p.Nodes {
			if len(n.States) > 0 && n.States[len(n.States)-1] == hAlive {
				alive++
			}
		}
		b.WriteString(styleTitle.Render("pool "+strings.Join(p.Masters, ", ")) + "  " + styleDim.Render(p.Refs) + "\n")
		loss := hMutedStyle.Render("pick never lost")
		if p.PickLoss > 0 {
			loss = hDeadStyle.Render(fmt.Sprintf("pick lost %d× (%s dead)", p.PickLoss, time.Duration(p.LossSec)*time.Second))
		}
		churn := fmt.Sprintf("churn %d%%", p.ChurnPct)
		switch {
		case p.ChurnPct >= 30:
			churn = hDeadStyle.Render(churn)
		case p.ChurnPct >= 10:
			churn = hMixedStyle.Render(churn)
		}
		strategy := ""
		if p.Strategy != "" {
			strategy = p.Strategy + " · "
		}
		if p.Auto {
			strategy = "auto → " + strategy
		}
		fmt.Fprintf(&b, "  %slast %s · %d/%d alive · %s · %s\n", strategy, shortDur(span), alive, len(p.Nodes), loss, churn)
		if p.Auto && p.AutoReason != "" && p.AutoSince > 0 {
			fmt.Fprintf(&b, "  %s\n", hMutedStyle.Render(fmt.Sprintf("%s since %s: %s", p.Strategy, time.Unix(p.AutoSince, 0).Format("15:04"), p.AutoReason)))
		}
		for _, n := range p.Nodes {
			b.WriteString(healthRow(n, nameW, stripW))
		}
	}
	b.WriteString("\n" + hMutedStyle.Render("● active  ○ spare   ") +
		hAliveStyle.Render("█") + hMutedStyle.Render(" alive  ") +
		hDeadStyle.Render("▁") + hMutedStyle.Render(" dead  ") +
		hMixedStyle.Render("▄") + hMutedStyle.Render(" both  ░ parked  · unknown  ") +
		hUplinkStyle.Render("×") + hMutedStyle.Render(" uplink down   oldest → now") + "\n")
	return b.String()
}

func healthRow(n *emxv1.PoolHealthNode, nameW, stripW int) string {
	marker := "  "
	switch n.Role {
	case "active":
		marker = hAliveStyle.Render("●") + " "
	case "spare":
		marker = "○ "
	}
	name := n.Name
	if name == "" {
		name = n.Key
	}
	// Node names often carry flag emoji: measure display width, not runes.
	if lipgloss.Width(name) > nameW {
		r := []rune(name)
		for len(r) > 0 && lipgloss.Width(string(r))+1 > nameW {
			r = r[:len(r)-1]
		}
		name = string(r) + "…"
	}
	name += strings.Repeat(" ", max(0, nameW-lipgloss.Width(name)))

	uptime := "   -"
	if n.UptimePct >= 0 {
		uptime = fmt.Sprintf("%3d%%", n.UptimePct)
	}
	flips := fmt.Sprintf("  %2d flips", n.Flips)
	if n.Flips >= 4 {
		flips = hDeadStyle.Render(flips)
	} else if n.Flips >= 2 {
		flips = hMixedStyle.Render(flips)
	}
	rtt := "        "
	switch {
	case n.Role == "parked":
		rtt = "  parked"
	case n.AvgRttMs > 0:
		rtt = fmt.Sprintf("  %4dms", n.AvgRttMs)
	}
	return marker + name + "  " + renderStrip(n.States, stripW) + "  " + uptime + flips + rtt + "\n"
}

// renderStrip draws states right-aligned into width columns, merging rounds
// when there are more than columns.
func renderStrip(states []byte, width int) string {
	cols := mergeStates(states, width)
	var b strings.Builder
	b.WriteString(strings.Repeat(" ", width-len(cols)))
	for _, st := range cols {
		switch st {
		case hAlive:
			b.WriteString(hAliveStyle.Render("█"))
		case hDead:
			b.WriteString(hDeadStyle.Render("▁"))
		case hMixed:
			b.WriteString(hMixedStyle.Render("▄"))
		case hParked:
			b.WriteString(hMutedStyle.Render("░"))
		case hUnknown:
			b.WriteString(hMutedStyle.Render("·"))
		case hUplink:
			b.WriteString(hUplinkStyle.Render("×"))
		default:
			b.WriteString(" ")
		}
	}
	return b.String()
}

// mergeStates folds states into at most width columns. A column holding both
// alive and dead rounds is hMixed — a flap inside the column must not vanish —
// and one touching an uplink outage shows the outage.
func mergeStates(states []byte, width int) []byte {
	if len(states) <= width {
		return states
	}
	out := make([]byte, width)
	for c := range out {
		lo, hi := c*len(states)/width, (c+1)*len(states)/width
		var alive, dead, parked, uplink, unknown bool
		for _, st := range states[lo:hi] {
			switch st {
			case hAlive:
				alive = true
			case hDead:
				dead = true
			case hParked:
				parked = true
			case hUplink:
				uplink = true
			case hUnknown:
				unknown = true
			}
		}
		switch {
		case alive && dead:
			out[c] = hMixed
		case uplink: // a local outage must not read as the nodes' fault
			out[c] = hUplink
		case alive:
			out[c] = hAlive
		case dead:
			out[c] = hDead
		case parked:
			out[c] = hParked
		case unknown:
			out[c] = hUnknown
		}
	}
	return out
}

func shortDur(d time.Duration) string {
	d = d.Round(time.Second)
	if d >= time.Minute {
		return d.Round(time.Minute).String()[:len(d.Round(time.Minute).String())-2]
	}
	return d.String()
}

// autoStrategyCmd shows or tunes the thresholds the auto strategy judges
// pools by.
func autoStrategyCmd() *cobra.Command {
	var window, calm time.Duration
	var flips, flapping, pickLoss int
	var defaults bool
	c := &cobra.Command{
		Use:   "auto-strategy",
		Short: "show or tune when auto pools switch between stable and agile",
		Long: "Pools whose subscriptions are on the auto strategy start stable. Every poll the\n" +
			"daemon judges each one over the last --window of health: it is flapping when\n" +
			"its whole pick (active nodes + spare) died at once --pick-loss times, or when\n" +
			"--flapping percent of its nodes (at least 2) flipped alive<->dead --flips times\n" +
			"or more. A flapping pool goes agile at once and back to stable only after\n" +
			"--calm without flapping. `emx health` shows each pool's flips and pick losses.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			f := cmd.Flags()
			changed := f.Changed("window") || f.Changed("flips") || f.Changed("flapping") || f.Changed("pick-loss") || f.Changed("calm")
			return withClient(cmd, func(ctx context.Context, cl emxv1.DaemonClient) error {
				cur, err := cl.AutoStrategy(ctx, &emxv1.AutoStrategyRequest{})
				if err != nil {
					return err
				}
				if defaults || changed {
					t := cur.Tuning
					if defaults {
						t = cur.Defaults
					}
					if f.Changed("window") {
						t.WindowMin = int32(window / time.Minute)
					}
					if f.Changed("flips") {
						t.Flips = int32(flips)
					}
					if f.Changed("flapping") {
						t.FlappingPct = int32(flapping)
					}
					if f.Changed("pick-loss") {
						t.PickLoss = int32(pickLoss)
					}
					if f.Changed("calm") {
						t.CalmMin = int32(calm / time.Minute)
					}
					if cur, err = cl.AutoStrategy(ctx, &emxv1.AutoStrategyRequest{Change: true, Set: t}); err != nil {
						return err
					}
				}
				t := cur.Tuning
				pick := fmt.Sprintf("its whole pick died %d× or more", t.PickLoss)
				if t.PickLoss == 0 {
					pick = "(pick losses not judged)"
				}
				fmt.Fprintf(cmd.OutOrStdout(),
					"auto pools go agile when, within %dm:\n  %s, or\n  %d%%+ of nodes (at least 2) flipped alive↔dead %d+ times\n"+
						"and back to stable after %dm without flapping\n",
					t.WindowMin, pick, t.FlappingPct, t.Flips, t.CalmMin)
				return nil
			})
		},
	}
	c.Flags().DurationVar(&window, "window", 0, "how much health history to judge by (2m-30m)")
	c.Flags().IntVar(&flips, "flips", 0, "alive<->dead flips that make a node flapping")
	c.Flags().IntVar(&flapping, "flapping", 0, "percent of nodes flapping that make the pool flapping")
	c.Flags().IntVar(&pickLoss, "pick-loss", 0, "whole-pick losses that make the pool flapping (0 = off)")
	c.Flags().DurationVar(&calm, "calm", 0, "time without flapping before an agile pool goes back to stable")
	c.Flags().BoolVar(&defaults, "defaults", false, "reset to the defaults (other flags then apply on top)")
	return c
}
