package main

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	emxv1 "github.com/ehsan200/em-xray/api/emxv1"
	"github.com/spf13/cobra"
)

// menuSession holds a live daemon connection for a run of interactive menus.
// cmd is kept so a daemon restart can respawn the detached process.
type menuSession struct {
	c   emxv1.DaemonClient
	cmd *cobra.Command
}

func call() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 15*time.Second)
}

// testCall is the long deadline for probe runs: each batch starts its own xray
// before its (concurrent) probes.
func testCall() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), testTimeout)
}

// runMenu opens a connection and dispatches to the requested section's menu.
func runMenu(cmd *cobra.Command, section string) error {
	if !interactive() {
		return cmd.Help() // non-tty: fall back to help/flags
	}
	ctx, cancel := context.WithTimeout(cmd.Context(), 5*time.Second)
	c, conn, err := dialReady(ctx)
	cancel()
	if err != nil {
		return errDaemon(err)
	}
	defer conn.Close()
	if version != "dev" {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		ping, pingErr := c.Ping(ctx, &emxv1.PingRequest{})
		cancel()
		if pingErr == nil && ping.Version != "" && ping.Version != "dev" && ping.Version != version {
			return fmt.Errorf("emx CLI is %s but the running daemon is %s; restart the same service/user that owns your configs (`sudo systemctl restart emx` for a system service, or `emx restart` for a user daemon)", version, ping.Version)
		}
	}
	s := &menuSession{c: c, cmd: cmd}
	switch section {
	case "main":
		s.mainMenu()
	case "in":
		s.inboundsMenu()
	case "sub":
		s.subsMenu()
	case "entry":
		s.entriesMenu()
	case "caddy":
		s.caddyMenu()
	}
	return nil
}

func (s *menuSession) mainMenu() {
	for {
		i, ok := runSelect("em-xray", []selectItem{
			{"Inbounds", "server listeners you expose"},
			{"Subscriptions", "node pools for masters"},
			{"Entries", "outbounds & masters"},
			{"Traffic", "per-inbound/outbound usage + charts"},
			{"Pool health", "per-node timeline: flaps, uptime, pick losses"},
			{"Templates", "view built-in inbound presets"},
			{"Caddy", "HTTPS frontend for XHTTP domains"},
			{"Status", "daemon & xray health"},
			{"Restart", "cycle xray or the whole daemon"},
			{"Quit", ""},
		})
		if !ok || i == 9 {
			return
		}
		switch i {
		case 0:
			s.inboundsMenu()
		case 1:
			s.subsMenu()
		case 2:
			s.entriesMenu()
		case 3:
			s.trafficView()
		case 4:
			s.healthView(nil)
		case 5:
			s.templatesView()
		case 6:
			s.caddyMenu()
		case 7:
			s.statusView()
		case 8:
			if s.restartMenu() {
				return // the daemon was restarted — this connection is dead
			}
		}
	}
}

// restartMenu offers the two recovery levers. Returns true when the menu must
// close: a daemon restart drops the socket this session is talking over.
func (s *menuSession) restartMenu() (quit bool) {
	i, ok := runSelect("Restart", []selectItem{
		{"Restart xray", "regenerate the config and cycle the child — fixes a wedged xray"},
		{"Restart daemon", "full restart; closes this menu"},
		{"← Back", ""},
	})
	if !ok || i == 2 {
		return false
	}
	if i == 0 {
		notify("restarting xray…")
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		reply, err := s.c.XrayRestart(ctx, &emxv1.Empty{})
		cancel()
		if err != nil {
			notify("error: %v", err)
		} else if reply.Running {
			notify("%s (pid %d)", reply.Message, reply.Pid)
		} else {
			notify("%s", reply.Message)
		}
		return false
	}
	if !confirm("Restart the daemon? (this closes the menu)") {
		return false
	}
	if err := restartDaemon(s.cmd); err != nil {
		notify("error: %v", err)
		return false
	}
	notify("daemon restarted — run `emx` again")
	return true
}

// ---- inbounds --------------------------------------------------------------

func (s *menuSession) inboundsMenu() {
	for {
		ctx, cancel := call()
		reply, err := s.c.InboundList(ctx, &emxv1.Empty{})
		cancel()
		if err != nil {
			notify("error: %v", err)
			return
		}
		items := make([]selectItem, 0, len(reply.Inbounds)+2)
		for _, in := range reply.Inbounds {
			endpoint := fmt.Sprintf(":%d", in.Port)
			if in.Port == 0 && in.Listen != "" {
				endpoint = in.Listen
			}
			items = append(items, selectItem{
				label: in.Name,
				desc:  fmt.Sprintf("%s %s → %s", in.Protocol, endpoint, in.Target),
			})
		}
		items = append(items, selectItem{label: "+ Add inbound"}, selectItem{label: "← Back"})

		i, ok := runSelect("Inbounds", items)
		if !ok || i == len(items)-1 {
			return
		}
		if i == len(items)-2 {
			s.inboundAdd()
			continue
		}
		s.inboundActions(reply.Inbounds[i])
	}
}

func (s *menuSession) inboundActions(in *emxv1.InboundInfo) {
	for {
		endpoint := fmt.Sprintf(":%d", in.Port)
		if in.Port == 0 && in.Listen != "" {
			endpoint = in.Listen
		}
		title := fmt.Sprintf("Inbound: %s (%s %s → %s)", in.Name, in.Protocol, endpoint, in.Target)
		i, ok := runSelect(title, []selectItem{
			{"Show client link", ""},
			{"Show QR code", ""},
			{"Manage users", "extra clients + byte caps"},
			{"Change target", "where its traffic egresses — now " + in.Target},
			{"Duplicate", ""},
			{"Edit JSON", ""},
			{"Remove", ""},
			{"← Back", ""},
		})
		if !ok || i == 7 {
			return
		}
		switch i {
		case 0:
			link := pickInboundLink(in)
			if link != "" {
				fmt.Println("\n" + link + "\n")
			} else {
				notify("(this protocol has no share link)")
			}
		case 1:
			link := pickInboundLink(in)
			if link != "" {
				fmt.Printf("\n%s\n\n%s\n%s\n", in.Name, renderQR(link), link)
			} else {
				notify("(this protocol has no share link)")
			}
		case 2:
			s.usersMenu(in)
		case 3:
			s.inboundRetarget(in)
		case 4:
			name, _ := runInput("Name for the copy", in.Name+" copy")
			ctx, cancel := call()
			reply, err := s.c.InboundDuplicate(ctx, &emxv1.DuplicateRequest{Id: in.Id, NewName: name})
			cancel()
			if err != nil {
				notify("error: %v", err)
			} else {
				notify("duplicated to %s (:%d)", reply.Inbound.Name, reply.Inbound.Port)
				if isCaddyInboundInfo(reply.Inbound) {
					if err := maybeApplyCaddy(s.cmd); err != nil {
						notify("Caddy apply failed: %v", err)
					}
				}
			}
		case 5:
			s.editConfig("inbound", in.Id, isCaddyInboundInfo(in))
		case 6:
			if confirm("Remove inbound " + in.Name + "?") {
				ctx, cancel := call()
				_, err := s.c.InboundRemove(ctx, &emxv1.IdRequest{Id: in.Id})
				cancel()
				if err != nil {
					notify("error: %v", err)
				} else {
					notify("removed %s", in.Name)
					if isCaddyInboundInfo(in) {
						if err := maybeApplyCaddy(s.cmd); err != nil {
							notify("Caddy apply failed: %v", err)
						}
					}
					return
				}
			}
		}
	}
}

// inboundRetarget changes where an inbound's traffic egresses.
func (s *menuSession) inboundRetarget(in *emxv1.InboundInfo) {
	target := s.pickTarget()
	if target == "" || target == in.Target {
		return
	}
	ctx, cancel := call()
	reply, err := s.c.InboundSetTarget(ctx, &emxv1.InboundTargetRequest{Id: in.Id, Target: target})
	cancel()
	if err != nil {
		notify("error: %v", err)
		return
	}
	in.Target = reply.Inbound.Target
	notify("%s now egresses via %s", in.Name, in.Target)
}

// pickInboundLink returns the link to show for an inbound. A public socks
// inbound has two forms (socks:// for proxy clients, tg://socks for Telegram),
// so it prompts the user to choose; otherwise it returns the single share link.
func pickInboundLink(in *emxv1.InboundInfo) string {
	if in.TgLink == "" {
		return in.ShareLink
	}
	i, ok := runSelect("Which link?", []selectItem{
		{"socks:// (proxy clients)", "xray / v2ray / sing-box"},
		{"tg://socks (Telegram)", "tap into Telegram proxy settings"},
	})
	if !ok {
		return ""
	}
	if i == 1 {
		return in.TgLink
	}
	return in.ShareLink
}

// usersMenu manages the extra clients of an inbound.
func (s *menuSession) usersMenu(in *emxv1.InboundInfo) {
	for {
		ctx, cancel := call()
		reply, err := s.c.InboundUserList(ctx, &emxv1.IdRequest{Id: in.Id})
		cancel()
		if err != nil {
			notify("error: %v", err)
			return
		}
		items := make([]selectItem, 0, len(reply.Users)+2)
		for _, u := range reply.Users {
			state := "enabled"
			if !u.Enabled {
				state = "disabled"
			}
			if u.OverCap {
				state = "OVER CAP"
			}
			desc := fmt.Sprintf("%s · used %s", state, humanBytes(u.UsedUp+u.UsedDown))
			if u.ByteCap > 0 {
				desc += " / " + humanBytes(u.ByteCap)
			}
			items = append(items, selectItem{label: u.Name, desc: desc})
		}
		items = append(items, selectItem{label: "+ Add user"}, selectItem{label: "← Back"})
		i, ok := runSelect("Users of "+in.Name, items)
		if !ok || i == len(items)-1 {
			return
		}
		if i == len(items)-2 {
			s.userAdd(in)
			continue
		}
		s.userActions(in, reply.Users[i])
	}
}

func (s *menuSession) userAdd(in *emxv1.InboundInfo) {
	name, ok := runInput("User name", "")
	if !ok || name == "" {
		return
	}
	capStr, _ := runInput("Byte cap (e.g. 10GB, blank = unlimited)", "")
	capBytes, err := parseSize(capStr)
	if err != nil {
		notify("bad cap: %v", err)
		return
	}
	ctx, cancel := call()
	reply, err := s.c.InboundUserAdd(ctx, &emxv1.InboundUserAddRequest{InboundId: in.Id, Name: name, ByteCap: capBytes})
	cancel()
	if err != nil {
		notify("error: %v", err)
		return
	}
	fmt.Printf("\n%s\n\n%s\n%s\n", reply.User.Name, renderQR(reply.User.ShareLink), reply.User.ShareLink)
}

func (s *menuSession) userActions(in *emxv1.InboundInfo, u *emxv1.UserInfo) {
	toggle := "Disable"
	if !u.Enabled {
		toggle = "Enable"
	}
	i, ok := runSelect(fmt.Sprintf("User: %s", u.Name), []selectItem{
		{"Show link", ""}, {"Show QR code", ""}, {toggle, ""}, {"Remove", ""}, {"← Back", ""},
	})
	if !ok || i == 4 {
		return
	}
	switch i {
	case 0:
		fmt.Println("\n" + u.ShareLink + "\n")
	case 1:
		fmt.Printf("\n%s\n\n%s\n%s\n", u.Name, renderQR(u.ShareLink), u.ShareLink)
	case 2:
		ctx, cancel := call()
		_, err := s.c.InboundUserSetEnabled(ctx, &emxv1.SetEnabledRequest{Id: u.Id, Enabled: !u.Enabled})
		cancel()
		if err != nil {
			notify("error: %v", err)
		} else {
			notify("%s %s", map[bool]string{true: "enabled", false: "disabled"}[!u.Enabled], u.Name)
		}
	case 3:
		if confirm("Remove user " + u.Name + "?") {
			ctx, cancel := call()
			_, err := s.c.InboundUserRemove(ctx, &emxv1.IdRequest{Id: u.Id})
			cancel()
			if err != nil {
				notify("error: %v", err)
			} else {
				notify("removed %s", u.Name)
			}
		}
	}
}

func (s *menuSession) inboundAdd() { s.inboundAddTo("") }

// inboundAddTo runs the add-inbound flow; a non-empty target skips the
// egress picker (used right after creating the outbound it should feed).
func (s *menuSession) inboundAddTo(target string) {
	name, ok := runInput("Name", "")
	if !ok || name == "" {
		return
	}
	// Template picker.
	ctx, cancel := call()
	tmpls, err := s.c.TemplateList(ctx, &emxv1.Empty{})
	cancel()
	if err != nil {
		notify("error: %v", err)
		return
	}
	titems := make([]selectItem, len(tmpls.Templates))
	for i, t := range tmpls.Templates {
		titems[i] = selectItem{label: t.Name, desc: t.Description}
	}
	ti, ok := runSelect("Template", titems)
	if !ok {
		return
	}
	template := tmpls.Templates[ti].Name

	if target == "" {
		target = s.pickTarget()
	}
	if target == "" {
		return
	}
	hostPrompt := "Public host/IP for the link (blank = auto-detect)"
	if template == "vless-caddy-xhttp" {
		hostPrompt = "Domain (for example x.example.com)"
	}
	host, _ := runInput(hostPrompt, "")
	path, email, mode := "", "", ""
	if template == "vless-caddy-xhttp" {
		if host == "" {
			notify("domain is required for the Caddy/XHTTP template")
			return
		}
		path, _ = runInput("XHTTP path (blank = random)", "")
		email, _ = runInput("Primary client email (blank = generated)", "")
		mode, _ = runInput("XHTTP mode", "packet-up")
	}

	ctx, cancel = call()
	reply, err := s.c.InboundAdd(ctx, &emxv1.InboundAddRequest{
		Name: name, Template: template, Target: target, PublicHost: host,
		Path: path, Email: email, XhttpMode: mode,
	})
	cancel()
	if err != nil {
		notify("error: %v", err)
		return
	}
	in := reply.Inbound
	notify("added %s: %s on :%d → %s", in.Name, in.Protocol, in.Port, in.Target)
	if in.ShareLink != "" {
		fmt.Println("\nclient link:\n  " + in.ShareLink + "\n")
	}
	if isCaddyInboundInfo(in) {
		if err := maybeApplyCaddy(s.cmd); err != nil {
			notify("Caddy apply failed: %v", err)
		}
	}
}

// ---- Caddy ----------------------------------------------------------------

func (s *menuSession) caddyMenu() {
	for {
		i, ok := runSelect("Caddy", []selectItem{
			{"Status", "installation and systemd state"},
			{"Domains", "domains routed to Caddy/XHTTP inbounds"},
			{"View generated Caddyfile", "preview without changing the server"},
			{"Apply configuration", "validate and reload Caddy"},
			{"Install Caddy", "official Debian/Ubuntu package; needs root"},
			{"Enable Caddy", "enable and start the system service; needs root"},
			{"Disable Caddy", "stop and disable the system service; needs root"},
			{"← Back", ""},
		})
		if !ok || i == 7 {
			return
		}
		var action *cobra.Command
		switch i {
		case 0:
			action = caddyStatusCmd()
		case 1:
			action = caddyDomainsCmd()
		case 2:
			action = caddyPrintCmd()
		case 3:
			action = caddyApplyCmd()
		case 4:
			action = caddyInstallCmd()
		case 5:
			action = caddyEnableCmd()
		case 6:
			action = caddyDisableCmd()
		}
		if action != nil {
			action.SetContext(caddyCommandContext(s.cmd))
			action.SetOut(s.cmd.OutOrStdout())
			action.SetErr(s.cmd.ErrOrStderr())
			if err := action.RunE(action, nil); err != nil {
				notify("error: %v", err)
			}
		}
	}
}

// pickTarget lets the user choose the egress: direct, a master, or an entry.
func (s *menuSession) pickTarget() string {
	ctx, cancel := call()
	entries, err := s.c.EntryList(ctx, &emxv1.Empty{})
	cancel()
	items := []selectItem{{label: "direct", desc: "egress straight out"}}
	var targets []string
	targets = append(targets, "direct")
	if err == nil {
		for _, e := range entries.Entries {
			if e.IsMaster {
				items = append(items, selectItem{label: "master:" + e.Name, desc: "through fastest node in its pool"})
				targets = append(targets, "master:"+e.Name)
			}
		}
		for _, e := range entries.Entries {
			if !e.IsMaster {
				items = append(items, selectItem{label: "xray:" + e.Name, desc: entryTargetDesc(e)})
				targets = append(targets, "xray:"+e.Name)
			}
		}
	}
	items = append(items, selectItem{label: "Network interface…", desc: "leave by a chosen interface (second uplink, VPN tunnel)"})
	i, ok := runSelect("Egress target", items)
	if !ok {
		return ""
	}
	if i == len(targets) {
		return s.pickInterface()
	}
	return targets[i]
}

// pickInterface lets the user choose one of the server's network interfaces
// and returns it as an iface:NAME target ("" on cancel).
func (s *menuSession) pickInterface() string {
	ctx, cancel := call()
	reply, err := s.c.Interfaces(ctx, &emxv1.Empty{})
	cancel()
	if err != nil {
		notify("error: %v", err)
		return ""
	}
	var items []selectItem
	var names []string
	for _, ifc := range reply.Interfaces {
		if ifc.Loopback {
			continue
		}
		desc := ifaceState(ifc)
		if len(ifc.Addrs) > 0 {
			desc += " · " + strings.Join(ifc.Addrs, " ")
		}
		items = append(items, selectItem{label: ifc.Name, desc: desc})
		names = append(names, ifc.Name)
	}
	if len(items) == 0 {
		notify("no network interfaces found")
		return ""
	}
	items = append(items, selectItem{label: "← Back"})
	i, ok := runSelect("Network interface", items)
	if !ok || i == len(names) {
		return ""
	}
	return "iface:" + names[i]
}

// ---- subscriptions ---------------------------------------------------------

func (s *menuSession) subsMenu() {
	for {
		ctx, cancel := call()
		reply, err := s.c.SubList(ctx, &emxv1.Empty{})
		cancel()
		if err != nil {
			notify("error: %v", err)
			return
		}
		items := make([]selectItem, 0, len(reply.Subs)+2)
		for _, sub := range reply.Subs {
			state := "enabled"
			if !sub.Enabled {
				state = "disabled"
			}
			meta := fmt.Sprintf("%s · %d nodes, %d active", state, sub.NodeCount, sub.ActiveCount)
			if u := usedLine(sub); u != "-" {
				meta += " · " + u
			}
			if e := expiryShort(sub.Expire); e != "-" {
				meta += " · exp " + e
			}
			items = append(items, selectItem{label: sub.Name, desc: meta})
		}
		items = append(items, selectItem{label: "+ Add subscription"}, selectItem{label: "← Back"})

		i, ok := runSelect("Subscriptions", items)
		if !ok || i == len(items)-1 {
			return
		}
		if i == len(items)-2 {
			s.subAdd()
			continue
		}
		s.subActions(reply.Subs[i])
	}
}

func (s *menuSession) subAdd() {
	name, ok := runInput("Name", "")
	if !ok || name == "" {
		return
	}
	url, ok := runInput("Subscription URL", "")
	if !ok || url == "" {
		return
	}
	notify("fetching…")
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	reply, err := s.c.SubAdd(ctx, &emxv1.SubAddRequest{Name: name, Url: url})
	cancel()
	if err != nil {
		notify("error: %v", err)
		return
	}
	notify("added %s: %d nodes, %d active", reply.Sub.Name, reply.Sub.NodeCount, reply.Sub.ActiveCount)
}

func (s *menuSession) subActions(sub *emxv1.SubInfo) {
	for {
		toggle := "Disable"
		if !sub.Enabled {
			toggle = "Enable"
		}
		i, ok := runSelect(fmt.Sprintf("Subscription: %s (%d active)", sub.Name, sub.ActiveCount), []selectItem{
			{"View metadata", ""},
			{"View / toggle nodes", ""},
			{"Test all nodes", "measure real latency through every node"},
			{"Pool health", "per-node timeline of the pools using it"},
			{"Switch strategy", strategyLine(sub.Strategy)},
			{"Refresh now", ""},
			{"Refresh interval / cap", ""},
			{toggle, ""},
			{"Rename", ""},
			{"Remove", ""},
			{"← Back", ""},
		})
		if !ok || i == 10 {
			return
		}
		switch i {
		case 0:
			fmt.Print("\n" + subMetaText(sub) + "\n")
		case 1:
			s.nodesMenu(sub)
		case 2:
			s.testSub(sub)
		case 3:
			name := sub.Name
			s.healthView(func(p *emxv1.PoolHealthPool) bool { return poolUsesSub(p, name) })
		case 4:
			s.subSetStrategy(sub)
		case 5:
			notify("refreshing…")
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			r, err := s.c.SubRefresh(ctx, &emxv1.SubRefreshRequest{Id: sub.Id})
			cancel()
			if err != nil {
				notify("error: %v", err)
			} else if r.Added == 0 && r.Removed == 0 {
				notify("refreshed: %d nodes (no change)", r.Nodes)
			} else {
				notify("refreshed: %d nodes (+%d -%d)", r.Nodes, r.Added, r.Removed)
			}
		case 6:
			s.subSetOptions(sub)
		case 7:
			ctx, cancel := call()
			_, err := s.c.SubSetEnabled(ctx, &emxv1.SetEnabledRequest{Id: sub.Id, Enabled: !sub.Enabled})
			cancel()
			if err != nil {
				notify("error: %v", err)
			} else {
				sub.Enabled = !sub.Enabled
			}
		case 8:
			name, ok := runInput("New name", sub.Name)
			if !ok || name == "" || name == sub.Name {
				break
			}
			ctx, cancel := call()
			_, err := s.c.SubRename(ctx, &emxv1.RenameRequest{Id: sub.Id, NewName: name})
			cancel()
			if err != nil {
				notify("error: %v", err)
			} else {
				notify("renamed to %s", name)
				sub.Name = name
			}
		case 9:
			if confirm("Remove subscription " + sub.Name + "?") {
				ctx, cancel := call()
				_, err := s.c.SubRemove(ctx, &emxv1.IdRequest{Id: sub.Id})
				cancel()
				if err != nil {
					notify("error: %v", err)
				} else {
					notify("removed %s", sub.Name)
					return
				}
			}
		}
	}
}

// subSetStrategy lets the user pick how pools drawing on sub switch nodes.
func (s *menuSession) subSetStrategy(sub *emxv1.SubInfo) {
	opts := []string{"auto", "stable", "agile", "manual"}
	i, ok := runSelect("Switch strategy for "+sub.Name, []selectItem{
		{"auto", "the daemon picks per pool"},
		{"stable", "sticky pick, parks dead nodes — for pools that stay up"},
		{"agile", "follows nodes that come and go in waves; closes dead nodes' connections"},
		{"manual", "only the nodes you pin (View / toggle nodes → Pin)"},
		{"← Back", ""},
	})
	if !ok || i == len(opts) {
		return
	}
	ctx, cancel := call()
	reply, err := s.c.SubSetOptions(ctx, &emxv1.SubOptionsRequest{
		Id: sub.Id, IntervalSec: sub.IntervalSec, NodeCap: sub.NodeCap, UserAgent: sub.UserAgent, Strategy: opts[i],
	})
	cancel()
	if err != nil {
		notify("error: %v", err)
		return
	}
	sub.Strategy = reply.Sub.Strategy
	notify("strategy: %s", strategyLine(sub.Strategy))
}

// subSetOptions prompts for a new refresh interval and node cap and applies them.
func (s *menuSession) subSetOptions(sub *emxv1.SubInfo) {
	ivInput, ok := runInput("Refresh interval seconds (0 = 12h default)", strconv.Itoa(int(sub.IntervalSec)))
	if !ok {
		return
	}
	iv, err := strconv.Atoi(strings.TrimSpace(ivInput))
	if err != nil || iv < 0 {
		notify("bad interval %q", ivInput)
		return
	}
	capInput, ok := runInput("Max active nodes (0 = 30 default)", strconv.Itoa(int(sub.NodeCap)))
	if !ok {
		return
	}
	cap, err := strconv.Atoi(strings.TrimSpace(capInput))
	if err != nil || cap < 0 {
		notify("bad cap %q", capInput)
		return
	}
	ctx, cancel := call()
	reply, err := s.c.SubSetOptions(ctx, &emxv1.SubOptionsRequest{
		Id: sub.Id, IntervalSec: int32(iv), NodeCap: int32(cap), UserAgent: sub.UserAgent,
	})
	cancel()
	if err != nil {
		notify("error: %v", err)
		return
	}
	sub.IntervalSec, sub.NodeCap = reply.Sub.IntervalSec, reply.Sub.NodeCap
	notify("updated: interval %s, cap %s", intervalLine(sub.IntervalSec), capLine(sub.NodeCap))
}

// testSub probes every node of a subscription and prints the table (latencies
// are persisted daemon-side, so the node list shows them afterwards).
func (s *menuSession) testSub(sub *emxv1.SubInfo) {
	notify("testing %s — this takes a while for a big pool…", sub.Name)
	ctx, cancel := testCall()
	reply, err := s.c.Test(ctx, &emxv1.TestRequest{Kind: "sub", Id: sub.Id})
	cancel()
	if err != nil {
		notify("error: %v", err)
		return
	}
	if len(reply.Results) == 0 {
		notify("no nodes — refresh the subscription first")
		return
	}
	fmt.Print("\n" + testResultLines(reply.Results, true) + "\n")
}

func (s *menuSession) nodesMenu(sub *emxv1.SubInfo) {
	for {
		ctx, cancel := call()
		reply, err := s.c.SubNodes(ctx, &emxv1.IdRequest{Id: sub.Id})
		cancel()
		if err != nil {
			notify("error: %v", err)
			return
		}
		if len(reply.Nodes) == 0 {
			notify("no nodes — refresh the subscription first")
			return
		}
		items := make([]selectItem, 0, len(reply.Nodes)+1)
		for _, n := range reply.Nodes {
			icon := "•"
			if n.Disabled {
				icon = "✗"
			} else if n.Active {
				icon = "✓"
			}
			lat := ""
			if n.LatencyMs > 0 {
				lat = fmt.Sprintf("%dms", n.LatencyMs)
			}
			if n.ParkedUntil != 0 {
				icon = "z"
				lat = strings.TrimSpace(lat + " " + parkedLabel(n.ParkedUntil))
			}
			if n.Rejected != "" {
				icon = "!"
				lat = "refused by xray: " + n.Rejected
			}
			if n.Pinned {
				icon = "📌"
			}
			items = append(items, selectItem{label: fmt.Sprintf("%s %s", icon, n.Name), desc: lat})
		}
		items = append(items, selectItem{label: "⚡ Test all nodes"}, selectItem{label: "← Back"})

		i, ok := runSelect(fmt.Sprintf("%s — nodes (✓ active · ✗ disabled · z parked dead · ! refused · 📌 pinned)", sub.Name), items)
		if !ok || i == len(items)-1 {
			return
		}
		if i == len(items)-2 {
			s.testSub(sub)
			continue
		}
		s.nodeActions(sub, reply.Nodes[i])
	}
}

// nodeActions is the per-node menu: test just this node, or toggle it.
func (s *menuSession) nodeActions(sub *emxv1.SubInfo, n *emxv1.NodeInfo) {
	toggle := "Disable"
	if n.Disabled {
		toggle = "Enable"
	}
	title := n.Name
	if n.LatencyMs > 0 {
		title += fmt.Sprintf(" (%dms)", n.LatencyMs)
	}
	pin, pinDesc := "Pin", "manual strategy routes only through pinned nodes"
	if n.Pinned {
		pin, pinDesc = "Unpin", ""
	}
	if sub.Strategy != "manual" && !n.Pinned {
		pinDesc = "takes effect once the strategy is manual"
	}
	i, ok := runSelect("Node: "+title, []selectItem{
		{"Test", "measure real latency through this node"},
		{toggle, "durable — survives a refresh"},
		{pin, pinDesc},
		{"← Back", ""},
	})
	if !ok || i == 3 {
		return
	}
	if i == 2 {
		ctx, cancel := call()
		_, err := s.c.SubSetNodePinned(ctx, &emxv1.SubNodePinnedRequest{SubId: sub.Id, Fingerprint: n.Fingerprint, Pinned: !n.Pinned})
		cancel()
		switch {
		case err != nil:
			notify("error: %v", err)
		case n.Pinned:
			notify("unpinned %s", n.Name)
		case sub.Strategy != "manual":
			notify("pinned %s — switch the strategy to manual to route only through pinned nodes", n.Name)
		default:
			notify("pinned %s", n.Name)
		}
		return
	}
	if i == 0 {
		notify("testing %s…", n.Name)
		ctx, cancel := testCall()
		reply, err := s.c.Test(ctx, &emxv1.TestRequest{Kind: "node", Id: sub.Id, Fingerprint: n.Fingerprint})
		cancel()
		if err != nil {
			notify("error: %v", err)
		} else {
			notify("%s", testSummary(reply.Results))
		}
		return
	}
	ctx, cancel := call()
	_, err := s.c.SubSetNodeDisabled(ctx, &emxv1.SubNodeDisabledRequest{
		SubId: sub.Id, Fingerprint: n.Fingerprint, Disabled: !n.Disabled,
	})
	cancel()
	if err != nil {
		notify("error: %v", err)
	} else if n.Disabled {
		notify("enabled %s", n.Name)
	} else {
		notify("disabled %s", n.Name)
	}
}

// ---- entries ---------------------------------------------------------------

func (s *menuSession) entriesMenu() {
	for {
		ctx, cancel := call()
		reply, err := s.c.EntryList(ctx, &emxv1.Empty{})
		cancel()
		if err != nil {
			notify("error: %v", err)
			return
		}
		items := make([]selectItem, 0, len(reply.Entries)+2)
		for _, e := range reply.Entries {
			kind := entryTargetDesc(e)
			if e.IsMaster {
				kind = "master → " + e.Dialer
			}
			items = append(items, selectItem{label: e.Name, desc: kind})
		}
		n := len(reply.Entries)
		if n > 0 {
			items = append(items, selectItem{label: "⚡ Test all entries", desc: "real latency through every entry"},
				selectItem{label: "⇄ Bulk change dialers", desc: "add/remove/replace dialer refs on several entries at once"})
		}
		items = append(items, selectItem{label: "+ Add entry / master"},
			selectItem{label: "+ Add Cloudflare WARP", desc: "free WARP account, registered for you — exit IP is Cloudflare's"},
			selectItem{label: "← Back"})

		i, ok := runSelect("Entries", items)
		switch {
		case !ok || i == len(items)-1:
			return
		case i == len(items)-2:
			s.warpAdd()
		case i == len(items)-3:
			s.entryAdd()
		case i == n:
			s.testEntries()
		case i == n+1:
			s.bulkDialer(reply.Entries)
		default:
			s.entryActions(reply.Entries[i])
		}
	}
}

// testEntries probes every entry and prints the table, like `emx entry test`.
func (s *menuSession) testEntries() {
	notify("testing all entries…")
	ctx, cancel := testCall()
	reply, err := s.c.Test(ctx, &emxv1.TestRequest{Kind: "entries"})
	cancel()
	if err != nil {
		notify("error: %v", err)
		return
	}
	fmt.Print("\n" + testResultLines(reply.Results, false) + "\n")
}

func (s *menuSession) entryActions(e *emxv1.EntryInfo) {
	kind := "entry"
	if e.IsMaster {
		kind = "master → " + e.Dialer
	}
	muxDesc := "mux: " + muxLabel(e)
	if e.MuxNote != "" {
		muxDesc += " — " + e.MuxNote
	}
	i, ok := runSelect(fmt.Sprintf("%s (%s)", e.Name, kind), []selectItem{
		{"Test", "measure real latency through this outbound"},
		{"Rename", ""}, {"Duplicate", ""}, {"Edit JSON", ""}, {"Remove", ""},
		{"Toggle mux", muxDesc}, {"Dialer", dialerDesc(e)}, {"← Back", ""},
	})
	if !ok || i == 7 {
		return
	}
	switch i {
	case 0:
		notify("testing %s…", e.Name)
		ctx, cancel := testCall()
		reply, err := s.c.Test(ctx, &emxv1.TestRequest{Kind: "entry", Id: e.Id})
		cancel()
		if err != nil {
			notify("error: %v", err)
		} else {
			notify("%s", testSummary(reply.Results))
		}
	case 1:
		name, ok := runInput("New name", e.Name)
		if !ok || name == "" || name == e.Name {
			return
		}
		ctx, cancel := call()
		_, err := s.c.EntryRename(ctx, &emxv1.RenameRequest{Id: e.Id, NewName: name})
		cancel()
		if err != nil {
			notify("error: %v", err)
		} else {
			notify("renamed to %s", name)
		}
	case 2:
		name, _ := runInput("Name for the copy", e.Name+" copy")
		ctx, cancel := call()
		reply, err := s.c.EntryDuplicate(ctx, &emxv1.DuplicateRequest{Id: e.Id, NewName: name})
		cancel()
		if err != nil {
			notify("error: %v", err)
		} else {
			notify("duplicated to %s", reply.Entry.Name)
		}
	case 3:
		s.editConfig("entry", e.Id, false)
	case 4:
		if confirm("Remove entry " + e.Name + "?") {
			ctx, cancel := call()
			_, err := s.c.EntryRemove(ctx, &emxv1.IdRequest{Id: e.Id})
			cancel()
			if err != nil {
				notify("error: %v", err)
			} else {
				notify("removed %s", e.Name)
			}
		}
	case 5:
		ctx, cancel := call()
		reply, err := s.c.EntrySetMux(ctx, &emxv1.SetEnabledRequest{Id: e.Id, Enabled: !e.Mux})
		cancel()
		if err != nil {
			notify("error: %v", err)
		} else {
			notify("%s: mux %s", e.Name, muxLabel(reply.Entry))
		}
	case 6:
		s.entrySetDialer(e)
	}
}

// dialerDesc summarizes an entry's dialer for the actions menu.
func dialerDesc(e *emxv1.EntryInfo) string {
	switch n := len(splitRefs(e.Dialer)); n {
	case 0:
		return "none — pick subscriptions/entries to make it a master"
	case 1:
		return e.Dialer
	default:
		return fmt.Sprintf("%d refs, one pool: %s", n, e.Dialer)
	}
}

// editConfig opens the given config (kind "inbound" or "entry") in $EDITOR and
// saves the result. It drops out of the TUI while the editor runs.
func (s *menuSession) editConfig(kind string, id uint32, caddyManaged bool) {
	ctx, cancel := call()
	var cur *emxv1.ConfigReply
	var err error
	if kind == "inbound" {
		cur, err = s.c.InboundGetConfig(ctx, &emxv1.IdRequest{Id: id})
	} else {
		cur, err = s.c.EntryGetConfig(ctx, &emxv1.IdRequest{Id: id})
	}
	cancel()
	if err != nil {
		notify("error: %v", err)
		return
	}
	edited, changed, err := editInEditor(cur.Json, ".json")
	if err != nil {
		notify("error: %v", err)
		return
	}
	if !changed {
		notify("no changes")
		return
	}
	ctx, cancel = call()
	var savedInbound *emxv1.InboundInfo
	if kind == "inbound" {
		var reply *emxv1.InboundReply
		reply, err = s.c.InboundSetConfig(ctx, &emxv1.SetConfigRequest{Id: id, Json: edited})
		if reply != nil {
			savedInbound = reply.Inbound
		}
	} else {
		_, err = s.c.EntrySetConfig(ctx, &emxv1.SetConfigRequest{Id: id, Json: edited})
	}
	cancel()
	if err != nil {
		notify("error: %v", err)
	} else {
		notify("saved")
		if caddyManaged || isCaddyInboundInfo(savedInbound) {
			if err := maybeApplyCaddy(s.cmd); err != nil {
				notify("Caddy apply failed: %v", err)
			}
		}
	}
}

func (s *menuSession) entryAdd() {
	name, ok := runInput("Name", "")
	if !ok || name == "" {
		return
	}
	link, ok := runInput("Share link (vless/vmess/trojan/ss/hysteria2)", "")
	if !ok || link == "" {
		return
	}
	// Optionally make it a master by choosing a dialer.
	dialer := ""
	if confirm("Make this a master (route through subscriptions/entries)?") {
		dialer, _ = s.pickDialer("", name)
	}
	ctx, cancel := call()
	reply, err := s.c.EntryAdd(ctx, &emxv1.EntryAddRequest{Name: name, Link: link, Dialer: dialer})
	cancel()
	if err != nil {
		notify("error: %v", err)
		return
	}
	kind := "entry"
	if reply.Entry.IsMaster {
		kind = "master"
	}
	notify("added %s %q", kind, reply.Entry.Name)
}

// dialerCandidates lists every subscription and entry a dialer can name, as
// checklist items and their refs. Entries named in skip are left out (a
// master can't route through itself).
func (s *menuSession) dialerCandidates(skip ...string) ([]selectItem, []string) {
	ctx, cancel := call()
	subs, _ := s.c.SubList(ctx, &emxv1.Empty{})
	entries, _ := s.c.EntryList(ctx, &emxv1.Empty{})
	cancel()
	var items []selectItem
	var refs []string
	if subs != nil {
		for _, sub := range subs.Subs {
			items = append(items, selectItem{label: "xraysub:" + sub.Name, desc: fmt.Sprintf("subscription · %d active nodes", sub.ActiveCount)})
			refs = append(refs, "xraysub:"+sub.Name)
		}
	}
	if entries != nil {
		for _, e := range entries.Entries {
			if slices.Contains(skip, e.Name) {
				continue
			}
			desc := "entry"
			if e.Protocol != "" {
				desc += " · " + e.Protocol
			}
			if e.IsMaster {
				desc += " · master → " + e.Dialer
			}
			items = append(items, selectItem{label: "xray:" + e.Name, desc: desc})
			refs = append(refs, "xray:"+e.Name)
		}
	}
	return items, refs
}

// pickDialer lets the user check any number of subscriptions and entries as
// the master's dialer; all checked refs feed one pool. cur pre-checks the
// existing refs, self (the master being edited) is left out. ok=false if
// cancelled or nothing is available.
func (s *menuSession) pickDialer(cur, self string) (string, bool) {
	items, refs := s.dialerCandidates(self)
	// Keep refs the lists don't know (e.g. a ref the daemon will reject) visible
	// so saving doesn't drop them silently.
	have := splitRefs(cur)
	for _, r := range have {
		if !slices.Contains(refs, r) {
			items = append(items, selectItem{label: r, desc: "not found"})
			refs = append(refs, r)
		}
	}
	if len(items) == 0 {
		notify("no subscriptions or entries yet — add one first")
		return "", false
	}
	checked := make([]bool, len(refs))
	for i, r := range refs {
		checked[i] = slices.Contains(have, r)
	}
	st, ok := runMultiSelect("Route through (check one or more — they merge into one pool)", items, checked)
	if !ok {
		return "", false
	}
	// Existing refs keep their order; newly checked ones follow.
	var out []string
	for _, r := range have {
		if st[slices.Index(refs, r)] {
			out = append(out, r)
		}
	}
	for i, r := range refs {
		if st[i] && !slices.Contains(out, r) {
			out = append(out, r)
		}
	}
	return strings.Join(out, ","), true
}

// bulkDialer changes the dialers of several entries at once: check the
// entries, pick an edit (add / remove / replace / clear), check the refs,
// confirm. The daemon validates all of them together and applies once.
func (s *menuSession) bulkDialer(entries []*emxv1.EntryInfo) {
	items := make([]selectItem, len(entries))
	checked := make([]bool, len(entries))
	for i, e := range entries {
		desc := "plain entry"
		if e.IsMaster {
			desc = "master → " + e.Dialer
		}
		items[i] = selectItem{label: e.Name, desc: desc}
		checked[i] = e.IsMaster
	}
	st, ok := runMultiSelect("Entries to change (masters pre-checked)", items, checked)
	if !ok {
		return
	}
	var sel []*emxv1.EntryInfo
	var names []string
	for i, on := range st {
		if on {
			sel = append(sel, entries[i])
			names = append(names, entries[i].Name)
		}
	}
	if len(sel) == 0 {
		notify("no entries checked")
		return
	}
	mode, ok := runSelect(fmt.Sprintf("%d entries — change their dialers how?", len(sel)), []selectItem{
		{"Add refs", "keep each one's refs, add the checked ones"},
		{"Remove refs", "drop the checked refs wherever they appear"},
		{"Replace with", "every entry gets exactly the checked refs"},
		{"Clear", "no dialer — they become plain entries"},
		{"← Back", ""},
	})
	if !ok || mode == 4 {
		return
	}
	var picked []string
	if mode != 3 {
		var refItems []selectItem
		var refs []string
		if mode == 1 { // only refs the selection actually has
			for _, e := range sel {
				for _, r := range splitRefs(e.Dialer) {
					if !slices.Contains(refs, r) {
						refs = append(refs, r)
						refItems = append(refItems, selectItem{label: r})
					}
				}
			}
		} else {
			refItems, refs = s.dialerCandidates(names...)
		}
		if len(refs) == 0 {
			notify("nothing to pick from")
			return
		}
		rs, ok := runMultiSelect("Refs", refItems, nil)
		if !ok {
			return
		}
		for i, on := range rs {
			if on {
				picked = append(picked, refs[i])
			}
		}
		if len(picked) == 0 {
			notify("no refs checked")
			return
		}
	}
	var set, add, rm []string
	switch mode {
	case 0:
		add = picked
	case 1:
		rm = picked
	case 2:
		set = picked
	}
	reqs, err := editDialers(sel, set, add, rm, mode == 3)
	if err != nil {
		notify("error: %v", err)
		return
	}
	var preview []string
	for i, r := range reqs {
		if r.Dialer != sel[i].Dialer {
			d := r.Dialer
			if d == "" {
				d = "(none)"
			}
			preview = append(preview, fmt.Sprintf("  %s → %s", sel[i].Name, d))
		}
	}
	if len(preview) == 0 {
		notify("no change")
		return
	}
	fmt.Print("\n" + strings.Join(preview, "\n") + "\n")
	if !confirm(fmt.Sprintf("Apply to %d entries?", len(preview))) {
		return
	}
	ctx, cancel := call()
	reply, err := s.c.EntryBulkDialer(ctx, &emxv1.EntryBulkDialerRequest{Items: reqs})
	cancel()
	if err != nil {
		notify("error: %v", err)
		return
	}
	notify("updated %d entries", len(reply.Entries))
}

// entrySetDialer changes (or clears) an entry's dialer from the checklist.
func (s *menuSession) entrySetDialer(e *emxv1.EntryInfo) {
	dialer, ok := s.pickDialer(e.Dialer, e.Name)
	if !ok || dialer == e.Dialer {
		return
	}
	if dialer == "" && !confirm("Nothing checked — make "+e.Name+" a plain entry (no dialer)?") {
		return
	}
	ctx, cancel := call()
	reply, err := s.c.EntrySetDialer(ctx, &emxv1.EntryDialerRequest{Id: e.Id, Dialer: dialer})
	cancel()
	switch {
	case err != nil:
		notify("error: %v", err)
	case reply.Entry.Dialer == "":
		notify("%s is now a plain entry", reply.Entry.Name)
	default:
		notify("%s → %s", reply.Entry.Name, reply.Entry.Dialer)
	}
}

// ---- read-only views -------------------------------------------------------

// healthView prints the pool health timeline (pools kept by keep, or all)
// above the menu.
func (s *menuSession) healthView(keep func(*emxv1.PoolHealthPool) bool) {
	ctx, cancel := call()
	reply, err := s.c.PoolHealth(ctx, &emxv1.PoolHealthRequest{})
	cancel()
	if err != nil {
		notify("error: %v", err)
		return
	}
	pools := reply.Pools
	if keep != nil {
		pools = filterPools(pools, keep)
	}
	fmt.Print("\n" + renderHealth(pools, termWidth()) + "\n")
}

// trafficView lets the user pick a window, then prints the traffic chart above
// the menu. Loops so windows can be switched without re-entering.
func (s *menuSession) trafficView() {
	windows := []struct {
		label string
		dur   time.Duration
	}{
		{"Last 24h", 24 * time.Hour},
		{"Last 48h", 48 * time.Hour},
		{"Last 7 days", 7 * 24 * time.Hour},
		{"All time", 8 * 24 * time.Hour},
	}
	for {
		items := make([]selectItem, len(windows)+1)
		for i, w := range windows {
			items[i] = selectItem{label: w.label}
		}
		items[len(items)-1] = selectItem{label: "← Back"}
		i, ok := runSelect("Traffic — pick a window", items)
		if !ok || i == len(items)-1 {
			return
		}
		ctx, cancel := call()
		reply, err := s.c.Traffic(ctx, &emxv1.TrafficRequest{WindowSec: int64(windows[i].dur.Seconds())})
		cancel()
		if err != nil {
			notify("error: %v", err)
			return
		}
		fmt.Print("\n" + renderTraffic(reply) + "\n")
	}
}

func (s *menuSession) templatesView() {
	ctx, cancel := call()
	reply, err := s.c.TemplateList(ctx, &emxv1.Empty{})
	cancel()
	if err != nil {
		notify("error: %v", err)
		return
	}
	items := make([]selectItem, len(reply.Templates)+1)
	for i, t := range reply.Templates {
		items[i] = selectItem{label: t.Name, desc: fmt.Sprintf("%s/%s/%s — %s", t.Protocol, t.Network, t.Security, t.Description)}
	}
	items[len(items)-1] = selectItem{label: "← Back"}
	runSelect("Templates", items)
}

func (s *menuSession) statusView() {
	ctx, cancel := call()
	st, err := s.c.Status(ctx, &emxv1.StatusRequest{})
	cancel()
	if err != nil {
		notify("error: %v", err)
		return
	}
	xray := "stopped"
	if st.Xray != nil && st.Xray.LastError != "" {
		xray += " — " + st.Xray.LastError
	}
	if st.Xray != nil && st.Xray.Running {
		health := "health pending"
		if st.Xray.HealthChecked && st.Xray.Responsive {
			health = "responsive"
		} else if st.Xray.HealthChecked {
			health = "degraded"
		}
		xray = fmt.Sprintf("running (pid %d, restarts %d, %s)", st.Xray.Pid, st.Xray.Restarts, health)
	}
	items := []selectItem{}
	// Current fastest node per master, if any.
	wctx, wcancel := call()
	if wins, err := s.c.Winners(wctx, &emxv1.Empty{}); err == nil {
		for _, w := range wins.Winners {
			node := w.Node
			if len(w.Nodes) > 0 {
				node = strings.Join(w.Nodes, ", ")
			}
			if node == "" {
				node = "(selecting…)"
			}
			items = append(items, selectItem{label: "master " + w.Master, desc: "via: " + node})
		}
	}
	wcancel()
	if st.UpdateAvailable {
		items = append(items, selectItem{label: "⬆ Update available: " + st.LatestVersion, desc: "run `emx update`"})
	}
	items = append(items, selectItem{label: "← Back"})
	runSelect(fmt.Sprintf("Status — daemon pid %d, up %ds · xray %s", st.DaemonPid, st.UptimeSec, xray), items)
}
