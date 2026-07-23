package main

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	emxv1 "github.com/ehsan200/em-xray/api/emxv1"
	"github.com/spf13/cobra"
)

// menuSession holds a live daemon connection for a run of interactive menus.
type menuSession struct{ c emxv1.DaemonClient }

func call() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 15*time.Second)
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
	s := &menuSession{c: c}
	switch section {
	case "main":
		s.mainMenu()
	case "in":
		s.inboundsMenu()
	case "sub":
		s.subsMenu()
	case "entry":
		s.entriesMenu()
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
			{"Templates", "view built-in inbound presets"},
			{"Status", "daemon & xray health"},
			{"Quit", ""},
		})
		if !ok || i == 6 {
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
			s.templatesView()
		case 5:
			s.statusView()
		}
	}
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
			items = append(items, selectItem{
				label: in.Name,
				desc:  fmt.Sprintf("%s :%d → %s", in.Protocol, in.Port, in.Target),
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
		title := fmt.Sprintf("Inbound: %s (%s :%d → %s)", in.Name, in.Protocol, in.Port, in.Target)
		i, ok := runSelect(title, []selectItem{
			{"Show client link", ""},
			{"Show QR code", ""},
			{"Manage users", "extra clients + byte caps"},
			{"Duplicate", ""},
			{"Edit JSON", ""},
			{"Remove", ""},
			{"← Back", ""},
		})
		if !ok || i == 6 {
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
			name, _ := runInput("Name for the copy", in.Name+" copy")
			ctx, cancel := call()
			reply, err := s.c.InboundDuplicate(ctx, &emxv1.DuplicateRequest{Id: in.Id, NewName: name})
			cancel()
			if err != nil {
				notify("error: %v", err)
			} else {
				notify("duplicated to %s (:%d)", reply.Inbound.Name, reply.Inbound.Port)
			}
		case 4:
			s.editConfig("inbound", in.Id)
		case 5:
			if confirm("Remove inbound " + in.Name + "?") {
				ctx, cancel := call()
				_, err := s.c.InboundRemove(ctx, &emxv1.IdRequest{Id: in.Id})
				cancel()
				if err != nil {
					notify("error: %v", err)
				} else {
					notify("removed %s", in.Name)
					return
				}
			}
		}
	}
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

func (s *menuSession) inboundAdd() {
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

	target := s.pickTarget()
	if target == "" {
		return
	}
	host, _ := runInput("Public host/IP for the link (blank = auto-detect)", "")

	ctx, cancel = call()
	reply, err := s.c.InboundAdd(ctx, &emxv1.InboundAddRequest{
		Name: name, Template: template, Target: target, PublicHost: host,
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
				items = append(items, selectItem{label: "xray:" + e.Name, desc: "through this entry"})
				targets = append(targets, "xray:"+e.Name)
			}
		}
	}
	i, ok := runSelect("Egress target", items)
	if !ok {
		return ""
	}
	return targets[i]
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
			{"Refresh now", ""},
			{"Refresh interval / cap", ""},
			{toggle, ""},
			{"Rename", ""},
			{"Remove", ""},
			{"← Back", ""},
		})
		if !ok || i == 7 {
			return
		}
		switch i {
		case 0:
			fmt.Print("\n" + subMetaText(sub) + "\n")
		case 1:
			s.nodesMenu(sub)
		case 2:
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
		case 3:
			s.subSetOptions(sub)
		case 4:
			ctx, cancel := call()
			_, err := s.c.SubSetEnabled(ctx, &emxv1.SetEnabledRequest{Id: sub.Id, Enabled: !sub.Enabled})
			cancel()
			if err != nil {
				notify("error: %v", err)
			} else {
				sub.Enabled = !sub.Enabled
			}
		case 5:
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
		case 6:
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
			items = append(items, selectItem{label: fmt.Sprintf("%s %s", icon, n.Name), desc: lat})
		}
		items = append(items, selectItem{label: "← Back"})

		i, ok := runSelect(fmt.Sprintf("%s — nodes (✓ active · ✗ disabled)", sub.Name), items)
		if !ok || i == len(items)-1 {
			return
		}
		n := reply.Nodes[i]
		ctx, cancel = call()
		_, err = s.c.SubSetNodeDisabled(ctx, &emxv1.SubNodeDisabledRequest{
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
			kind := "entry"
			if e.IsMaster {
				kind = "master → " + e.Dialer
			}
			items = append(items, selectItem{label: e.Name, desc: kind})
		}
		items = append(items, selectItem{label: "+ Add entry / master"}, selectItem{label: "← Back"})

		i, ok := runSelect("Entries", items)
		if !ok || i == len(items)-1 {
			return
		}
		if i == len(items)-2 {
			s.entryAdd()
			continue
		}
		s.entryActions(reply.Entries[i])
	}
}

func (s *menuSession) entryActions(e *emxv1.EntryInfo) {
	kind := "entry"
	if e.IsMaster {
		kind = "master → " + e.Dialer
	}
	i, ok := runSelect(fmt.Sprintf("%s (%s)", e.Name, kind), []selectItem{
		{"Rename", ""}, {"Duplicate", ""}, {"Edit JSON", ""}, {"Remove", ""}, {"← Back", ""},
	})
	if !ok || i == 4 {
		return
	}
	switch i {
	case 0:
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
	case 1:
		name, _ := runInput("Name for the copy", e.Name+" copy")
		ctx, cancel := call()
		reply, err := s.c.EntryDuplicate(ctx, &emxv1.DuplicateRequest{Id: e.Id, NewName: name})
		cancel()
		if err != nil {
			notify("error: %v", err)
		} else {
			notify("duplicated to %s", reply.Entry.Name)
		}
	case 2:
		s.editConfig("entry", e.Id)
	case 3:
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
	}
}

// editConfig opens the given config (kind "inbound" or "entry") in $EDITOR and
// saves the result. It drops out of the TUI while the editor runs.
func (s *menuSession) editConfig(kind string, id uint32) {
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
	if kind == "inbound" {
		_, err = s.c.InboundSetConfig(ctx, &emxv1.SetConfigRequest{Id: id, Json: edited})
	} else {
		_, err = s.c.EntrySetConfig(ctx, &emxv1.SetConfigRequest{Id: id, Json: edited})
	}
	cancel()
	if err != nil {
		notify("error: %v", err)
	} else {
		notify("saved")
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
	if confirm("Make this a master (route through a subscription/entry)?") {
		dialer = s.pickDialer()
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

// pickDialer builds a dialer ref by selecting a subscription or entry.
func (s *menuSession) pickDialer() string {
	ctx, cancel := call()
	subs, _ := s.c.SubList(ctx, &emxv1.Empty{})
	cancel()
	var items []selectItem
	var refs []string
	if subs != nil {
		for _, sub := range subs.Subs {
			items = append(items, selectItem{label: "xraysub:" + sub.Name, desc: fmt.Sprintf("%d active nodes", sub.ActiveCount)})
			refs = append(refs, "xraysub:"+sub.Name)
		}
	}
	if len(items) == 0 {
		notify("no subscriptions yet — add one first")
		return ""
	}
	i, ok := runSelect("Route through", items)
	if !ok {
		return ""
	}
	return refs[i]
}

// ---- read-only views -------------------------------------------------------

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
	if st.Xray != nil && st.Xray.Running {
		xray = fmt.Sprintf("running (pid %d, restarts %d)", st.Xray.Pid, st.Xray.Restarts)
	}
	items := []selectItem{}
	// Current fastest node per master, if any.
	wctx, wcancel := call()
	if wins, err := s.c.Winners(wctx, &emxv1.Empty{}); err == nil {
		for _, w := range wins.Winners {
			node := w.Node
			if node == "" {
				node = "(selecting…)"
			}
			items = append(items, selectItem{label: "master " + w.Master, desc: "fastest: " + node})
		}
	}
	wcancel()
	if st.UpdateAvailable {
		items = append(items, selectItem{label: "⬆ Update available: " + st.LatestVersion, desc: "run `emx update`"})
	}
	items = append(items, selectItem{label: "← Back"})
	runSelect(fmt.Sprintf("Status — daemon pid %d, up %ds · xray %s", st.DaemonPid, st.UptimeSec, xray), items)
}
