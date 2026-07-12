package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
	emxv1 "github.com/ehsan200/em-xray/api/emxv1"
	"github.com/spf13/cobra"
)

func trafficCmd() *cobra.Command {
	var window string
	c := &cobra.Command{
		Use:     "traffic",
		Short:   "show per-inbound/outbound traffic (totals + 24h chart)",
		Aliases: []string{"stats", "tr"},
		RunE: func(cmd *cobra.Command, _ []string) error {
			win, err := parseWindow(window)
			if err != nil {
				return err
			}
			return withClient(cmd, func(ctx context.Context, cl emxv1.DaemonClient) error {
				reply, err := cl.Traffic(ctx, &emxv1.TrafficRequest{WindowSec: int64(win.Seconds())})
				if err != nil {
					return err
				}
				fmt.Fprint(cmd.OutOrStdout(), renderTraffic(reply))
				return nil
			})
		},
	}
	c.Flags().StringVar(&window, "window", "24h", "chart window: a duration (24h, 48h), '7d', or 'all'")
	return c
}

func speedCmd() *cobra.Command {
	var interval time.Duration
	c := &cobra.Command{
		Use:     "speed",
		Short:   "live throughput per inbound/outbound (↑/↓ per second); Ctrl-C to stop",
		Aliases: []string{"live", "bw"},
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt)
			defer stop()
			c, conn, err := dialReady(ctx)
			if err != nil {
				return errDaemon(err)
			}
			defer conn.Close()
			return runSpeed(ctx, cmd, c, interval)
		},
	}
	c.Flags().DurationVar(&interval, "interval", time.Second, "refresh interval")
	return c
}

// runSpeed polls TrafficLive and renders a live rate table, redrawing in place.
func runSpeed(ctx context.Context, cmd *cobra.Command, c emxv1.DaemonClient, interval time.Duration) error {
	type sample struct {
		up, down int64
	}
	prev := map[string]sample{}
	last := time.Now()
	tk := time.NewTicker(interval)
	defer tk.Stop()
	out := cmd.OutOrStdout()
	first := true

	poll := func() error {
		rctx, cancel := context.WithTimeout(ctx, 3*time.Second)
		reply, err := c.TrafficLive(rctx, &emxv1.Empty{})
		cancel()
		if err != nil {
			return err
		}
		now := time.Now()
		dt := now.Sub(last).Seconds()
		last = now

		type rate struct {
			kind, name       string
			upRate, downRate float64
		}
		var rates []rate
		var maxRate float64
		for _, it := range reply.Items {
			key := it.Kind + "|" + it.Name
			p, seen := prev[key]
			prev[key] = sample{it.Up, it.Down}
			if !seen || first || dt <= 0 {
				continue // need two samples to compute a rate
			}
			ur := float64(it.Up-p.up) / dt
			dr := float64(it.Down-p.down) / dt
			if ur < 0 {
				ur = 0
			}
			if dr < 0 {
				dr = 0
			}
			rates = append(rates, rate{it.Kind, it.Name, ur, dr})
			if dr+ur > maxRate {
				maxRate = dr + ur
			}
		}
		first = false
		sort.Slice(rates, func(i, j int) bool {
			if rates[i].kind != rates[j].kind {
				return rates[i].kind < rates[j].kind
			}
			return rates[i].downRate+rates[i].upRate > rates[j].downRate+rates[j].upRate
		})

		fmt.Fprint(out, "\033[H\033[2J") // home + clear
		fmt.Fprintln(out, trafHeader.Render("Live throughput")+trafDim.Render("  · Ctrl-C to stop"))
		fmt.Fprintln(out)
		if len(rates) == 0 {
			fmt.Fprintln(out, trafDim.Render("  (measuring… or no traffic flowing)"))
			return nil
		}
		nameW := 4
		for _, r := range rates {
			if n := len([]rune(r.name)); n > nameW {
				nameW = n
			}
		}
		if nameW > 20 {
			nameW = 20
		}
		curKind := ""
		for _, r := range rates {
			if r.kind != curKind {
				curKind = r.kind
				fmt.Fprintln(out, trafHeader.Render(strings.ToUpper(r.kind)+"S"))
			}
			name := padRight(truncMiddle(r.name, nameW), nameW)
			bar := magBar(int64(r.downRate+r.upRate), int64(maxRate), 14)
			fmt.Fprintf(out, "  %s %s  %s %s\n",
				trafHeader.Render(name), bar,
				trafDown.Render(fmt.Sprintf("↓%s/s", humanBytes(int64(r.downRate)))),
				trafUp.Render(fmt.Sprintf("↑%s/s", humanBytes(int64(r.upRate)))))
		}
		return nil
	}

	if err := poll(); err != nil {
		return err
	}
	for {
		select {
		case <-ctx.Done():
			fmt.Fprintln(out)
			return nil
		case <-tk.C:
			if err := poll(); err != nil {
				return err
			}
		}
	}
}

// parseWindow accepts a Go duration, an "Nd" days form, or "all".
func parseWindow(s string) (time.Duration, error) {
	s = strings.TrimSpace(strings.ToLower(s))
	switch s {
	case "", "24h", "day":
		return 24 * time.Hour, nil
	case "all", "max":
		return 8 * 24 * time.Hour, nil
	}
	if strings.HasSuffix(s, "d") {
		n, err := strconv.Atoi(strings.TrimSuffix(s, "d"))
		if err != nil || n <= 0 {
			return 0, fmt.Errorf("bad window %q", s)
		}
		return time.Duration(n) * 24 * time.Hour, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil || d <= 0 {
		return 0, fmt.Errorf("bad window %q (try 24h, 7d, all)", s)
	}
	return d, nil
}

var (
	trafHeader = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("39"))
	trafDim    = lipgloss.NewStyle().Foreground(lipgloss.Color("241"))
	trafDown   = lipgloss.NewStyle().Foreground(lipgloss.Color("42"))  // green
	trafUp     = lipgloss.NewStyle().Foreground(lipgloss.Color("212")) // pink
	sparkStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("39"))
)

// renderTraffic renders both sections with totals, a magnitude bar and a
// per-hour sparkline.
func renderTraffic(r *emxv1.TrafficReply) string {
	if len(r.Inbounds) == 0 && len(r.Outbounds) == 0 {
		return trafDim.Render("no traffic recorded yet — it accrues while xray is running and moving bytes.") + "\n"
	}
	win := compactDur(time.Duration(r.WindowSec) * time.Second)
	var b strings.Builder
	b.WriteString(section("INBOUNDS", win, r.Inbounds))
	b.WriteString("\n")
	b.WriteString(section("OUTBOUNDS", win, r.Outbounds))
	return b.String()
}

func section(title, win string, items []*emxv1.TrafficItem) string {
	var b strings.Builder
	b.WriteString(trafHeader.Render(title) + trafDim.Render("  · last "+win) + "\n")
	if len(items) == 0 {
		b.WriteString(trafDim.Render("  (none)") + "\n")
		return b.String()
	}
	// Column widths + section max for the magnitude bar.
	nameW := 4
	var maxWin int64
	for _, it := range items {
		if n := len([]rune(it.Name)); n > nameW {
			nameW = n
		}
		if w := it.WindowDown + it.WindowUp; w > maxWin {
			maxWin = w
		}
	}
	if nameW > 20 {
		nameW = 20
	}
	for _, it := range items {
		name := padRight(truncMiddle(it.Name, nameW), nameW)
		bar := magBar(it.WindowDown+it.WindowUp, maxWin, 14)
		win := trafDown.Render("↓"+humanBytes(it.WindowDown)) + " " + trafUp.Render("↑"+humanBytes(it.WindowUp))
		win = lipgloss.NewStyle().Width(22).Render(win) // ANSI-aware padding
		all := trafDim.Render(fmt.Sprintf("all ↓%s ↑%s", humanBytes(it.TotalDown), humanBytes(it.TotalUp)))
		b.WriteString(fmt.Sprintf("  %s %s  %s %s\n", trafHeader.Render(name), bar, win, all))
		b.WriteString("  " + strings.Repeat(" ", nameW) + " " + sparkline(it.HourlyDown) + "\n")
	}
	return b.String()
}

// sparkline renders values as a colored unicode bar graph scaled to the max.
func sparkline(vals []int64) string {
	if len(vals) == 0 {
		return trafDim.Render("(no data)")
	}
	// A continuous baseline (▁ for zero) reads more like a chart than gaps.
	const ramp = "▁▂▃▄▅▆▇█"
	runes := []rune(ramp)
	var max int64
	for _, v := range vals {
		if v > max {
			max = v
		}
	}
	var sb strings.Builder
	for _, v := range vals {
		idx := 0
		if max > 0 && v > 0 {
			idx = int(int64(len(runes)-1) * v / max)
		}
		sb.WriteRune(runes[idx])
	}
	return sparkStyle.Render(sb.String())
}

// magBar renders a proportional horizontal bar (filled ∝ v/max).
func magBar(v, max int64, width int) string {
	filled := 0
	if max > 0 {
		filled = int(int64(width) * v / max)
	}
	if v > 0 && filled == 0 {
		filled = 1
	}
	return trafDown.Render(strings.Repeat("█", filled)) + trafDim.Render(strings.Repeat("░", width-filled))
}

func padRight(s string, n int) string {
	if d := n - len([]rune(s)); d > 0 {
		return s + strings.Repeat(" ", d)
	}
	return s
}
