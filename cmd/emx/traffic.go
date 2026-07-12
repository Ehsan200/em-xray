package main

import (
	"context"
	"fmt"
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
