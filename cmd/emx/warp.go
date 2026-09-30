package main

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	emxv1 "github.com/ehsan200/em-xray/api/emxv1"
	"github.com/ehsan200/em-xray/internal/warp"
	"github.com/spf13/cobra"
)

// defaultWarpName is the entry name `emx warp add` uses when none is given.
const defaultWarpName = "warp"

func warpCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "warp",
		Short: "Cloudflare WARP as an outbound (exit IP is Cloudflare's)",
		RunE:  func(cmd *cobra.Command, _ []string) error { return cmd.Help() },
	}
	c.AddCommand(warpAddCmd())
	return c
}

func warpAddCmd() *cobra.Command {
	var license, dialer, proxy, proxyUser, proxyPass string
	var inbound, template string
	c := &cobra.Command{
		Use:   "add [name]",
		Short: "register a new WARP account and add it as an entry (default name: warp)",
		Long: `Registers a fresh WARP device with Cloudflare and stores it as a WireGuard
entry. Route an inbound to it and that inbound's traffic exits from a
Cloudflare IP:

  emx warp add
  emx in add gate --to xray:warp

or in one go:  emx warp add --inbound gate`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := defaultWarpName
			if len(args) == 1 {
				name = args[0]
			}
			hc, err := proxyHTTPClient(cmd, proxy, proxyUser, proxyPass)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			fmt.Fprintln(out, "registering a WARP device with Cloudflare…")
			var e *emxv1.EntryInfo
			var acctType string
			err = withClientTimeout(cmd, 60*time.Second, func(ctx context.Context, cl emxv1.DaemonClient) error {
				e, acctType, err = addWarp(ctx, cl, hc, name, license, dialer)
				return err
			})
			if err != nil {
				return err
			}
			fmt.Fprintf(out, "added WARP entry %q (id %d, %s account)\n", e.Name, e.Id, acctType)
			if inbound == "" {
				fmt.Fprintf(out, "route an inbound to it:  emx in add <name> --to xray:%s\n", e.Name)
				return nil
			}
			var added *emxv1.InboundInfo
			err = withClient(cmd, func(ctx context.Context, cl emxv1.DaemonClient) error {
				reply, err := cl.InboundAdd(ctx, &emxv1.InboundAddRequest{
					Name: inbound, Template: template, Target: "xray:" + e.Name,
				})
				if err != nil {
					return fmt.Errorf("WARP entry added, but the inbound failed: %w", err)
				}
				added = reply.Inbound
				return nil
			})
			if err != nil {
				return err
			}
			printInbound(out, added, true)
			if isCaddyInboundInfo(added) {
				return maybeApplyCaddy(cmd)
			}
			return nil
		},
	}
	c.Flags().StringVar(&license, "license", "", "WARP+ license key to apply to the new device")
	c.Flags().StringVar(&dialer, "dialer", "",
		"reach WARP through a pool (xraysub:NAME / xray:NAME) instead of straight from this box")
	c.Flags().StringVar(&inbound, "inbound", "", "also create an inbound with this `NAME` routed to the WARP entry")
	c.Flags().StringVarP(&template, "template", "t", "", "template for --inbound (default: the server default)")
	c.Flags().StringVar(&proxy, "proxy", "",
		"register through a proxy when Cloudflare's API is unreachable from this box: a socks inbound "+
			"`NAME`, a HOST:PORT, or a full http:// / socks5h:// URL ["+envProxy+"]")
	c.Flags().StringVar(&proxyUser, "proxy-user", "", "`username` for --proxy ["+envUser+"]")
	c.Flags().StringVar(&proxyPass, "proxy-pass", "", "`password` for --proxy ["+envPass+"]")
	return c
}

// addWarp registers a WARP device and stores it as an entry. Registration runs
// here, in the CLI, so it can use the caller's proxy; the daemon only ever
// sees the finished outbound.
func addWarp(ctx context.Context, cl emxv1.DaemonClient, hc *http.Client, name, license, dialer string) (*emxv1.EntryInfo, string, error) {
	acc, err := warp.Register(ctx, hc, license)
	if err != nil {
		return nil, "", err
	}
	ob, err := acc.WireGuard.Outbound()
	if err != nil {
		return nil, "", err
	}
	reply, err := cl.EntryAdd(ctx, &emxv1.EntryAddRequest{
		Name: name, OutboundJson: string(ob), Dialer: dialer, Enabled: true,
	})
	if err != nil {
		return nil, "", err
	}
	return reply.Entry, acc.AccountType, nil
}

// entryTargetDesc describes what routing to an entry does, for pickers.
func entryTargetDesc(e *emxv1.EntryInfo) string {
	switch strings.ToLower(e.Protocol) {
	case "wireguard":
		return "WARP / WireGuard — exits from its peer's IP"
	case "":
		return "through this entry"
	default:
		return e.Protocol + " — through this entry"
	}
}

// warpAdd is the menu flow: name → register → offer an inbound routed to it.
func (s *menuSession) warpAdd() {
	name, ok := runInput("Name", defaultWarpName)
	if !ok {
		return
	}
	if name = strings.TrimSpace(name); name == "" {
		name = defaultWarpName
	}
	license, ok := runInput("WARP+ license key (blank = free)", "")
	if !ok {
		return
	}
	hc, err := proxyHTTPClient(s.cmd, "", "", "")
	if err != nil {
		notify("error: %v", err)
		return
	}
	notify("registering a WARP device with Cloudflare…")
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	e, acctType, err := addWarp(ctx, s.c, hc, name, license, "")
	cancel()
	if err != nil {
		notify("error: %v (if Cloudflare is blocked here: emx warp add --proxy <socks inbound>)", err)
		return
	}
	notify("added WARP entry %q (%s account)", e.Name, acctType)
	if confirm("Create an inbound that exits through it now?") {
		s.inboundAddTo("xray:" + e.Name)
	}
}
