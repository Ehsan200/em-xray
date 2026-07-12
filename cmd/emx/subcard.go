package main

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"
	emxv1 "github.com/ehsan200/em-xray/api/emxv1"
)

// Card styling. Colors match the palette in tui.go (39 blue, 212 pink, 241 dim).
var (
	cardBorder = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(lipgloss.Color("39")).
			Padding(0, 1)
	cardTitle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("39"))
	cardLabel = lipgloss.NewStyle().Foreground(lipgloss.Color("241"))
	cardRule  = lipgloss.NewStyle().Foreground(lipgloss.Color("238"))
	okBadge   = lipgloss.NewStyle().Foreground(lipgloss.Color("42")).Bold(true)
	offBadge  = lipgloss.NewStyle().Foreground(lipgloss.Color("241")).Bold(true)
	warnStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("214"))
	badStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("196"))
	goodStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("42"))
)

// renderSubCard renders a subscription's metadata as a styled, bordered card.
func renderSubCard(s *emxv1.SubInfo) string {
	badge := okBadge.Render("● enabled")
	if !s.Enabled {
		badge = offBadge.Render("○ disabled")
	}
	rows := []string{
		row("Nodes", fmt.Sprintf("%d (%d active)", s.NodeCount, s.ActiveCount)),
		row("Traffic", fmt.Sprintf("↑%s  ↓%s", humanBytes(s.Upload), humanBytes(s.Download))),
		row("Quota", quotaLine(s)),
		row("Expires", colorExpiry(s.Expire)),
		row("Fetched", colorFetch(s.LastFetched)),
		row("Interval", intervalLine(s.IntervalSec)),
		row("Node cap", capLine(s.NodeCap)),
		row("Agent", orDash(s.UserAgent)),
		row("URL", truncMiddle(s.Url, 46)),
	}
	if s.LastError != "" {
		rows = append(rows, row("Error", badStyle.Render(truncMiddle(s.LastError, 46))))
	}

	body := strings.Join(rows, "\n")
	header := cardTitle.Render(s.Name) + "  " + badge
	inner := lipgloss.Width(body)
	if w := lipgloss.Width(header); w > inner {
		inner = w
	}
	rule := cardRule.Render(strings.Repeat("─", inner))
	return cardBorder.Render(header + "\n" + rule + "\n" + body)
}

// row renders a fixed-width dim label followed by its value.
func row(label, value string) string {
	return cardLabel.Render(fmt.Sprintf("%-9s", label)) + " " + value
}

// quotaLine renders a colored usage bar + used/total, or a dash when no quota.
func quotaLine(s *emxv1.SubInfo) string {
	used := s.Upload + s.Download
	if s.Total <= 0 {
		if used > 0 {
			return fmt.Sprintf("%s used", humanBytes(used))
		}
		return cardLabel.Render("—")
	}
	const width = 16
	frac := float64(used) / float64(s.Total)
	if frac > 1 {
		frac = 1
	}
	filled := int(frac*float64(width) + 0.5)
	style := goodStyle
	switch {
	case frac >= 0.9:
		style = badStyle
	case frac >= 0.7:
		style = warnStyle
	}
	bar := style.Render(strings.Repeat("█", filled) + strings.Repeat("░", width-filled))
	return fmt.Sprintf("%s %.0f%%  %s / %s", bar, frac*100, humanBytes(used), humanBytes(s.Total))
}

func colorExpiry(expire int64) string {
	s := expiryLine(expire)
	if expire <= 0 {
		return cardLabel.Render(s)
	}
	switch {
	case strings.Contains(s, "EXPIRED"):
		return badStyle.Render(s)
	case daysLeft(expire) < 7:
		return badStyle.Render(s)
	case daysLeft(expire) < 30:
		return warnStyle.Render(s)
	}
	return s
}

func colorFetch(last int64) string {
	if last <= 0 {
		return warnStyle.Render("never")
	}
	return fetchLine(last)
}

// truncMiddle shortens s to max runes, keeping head + tail with an ellipsis.
func truncMiddle(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	if max < 5 {
		return string(r[:max])
	}
	head := (max - 1) / 2
	tail := max - 1 - head
	return string(r[:head]) + "…" + string(r[len(r)-tail:])
}
