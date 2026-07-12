package main

import (
	"context"
	"fmt"
	"time"

	emxv1 "github.com/gravisun/em-xray/api/emxv1"
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
			{"Templates", "view built-in inbound presets"},
			{"Status", "daemon & xray health"},
			{"Quit", ""},
		})
		if !ok || i == 5 {
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
			s.templatesView()
		case 4:
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
			{"Remove", ""},
			{"← Back", ""},
		})
		if !ok || i == 2 {
			return
		}
		switch i {
		case 0:
			if in.ShareLink != "" {
				fmt.Println("\n" + in.ShareLink + "\n")
			} else {
				notify("(this protocol has no share link)")
			}
		case 1:
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
			items = append(items, selectItem{
				label: sub.Name,
				desc:  fmt.Sprintf("%s · %d nodes, %d active", state, sub.NodeCount, sub.ActiveCount),
			})
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
			{"View / toggle nodes", ""},
			{"Refresh now", ""},
			{toggle, ""},
			{"Rename", ""},
			{"Remove", ""},
			{"← Back", ""},
		})
		if !ok || i == 5 {
			return
		}
		switch i {
		case 0:
			s.nodesMenu(sub)
		case 1:
			notify("refreshing…")
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			r, err := s.c.SubRefresh(ctx, &emxv1.SubRefreshRequest{Id: sub.Id})
			cancel()
			if err != nil {
				notify("error: %v", err)
			} else {
				notify("refreshed: %d nodes", r.Nodes)
			}
		case 2:
			ctx, cancel := call()
			_, err := s.c.SubSetEnabled(ctx, &emxv1.SetEnabledRequest{Id: sub.Id, Enabled: !sub.Enabled})
			cancel()
			if err != nil {
				notify("error: %v", err)
			} else {
				sub.Enabled = !sub.Enabled
			}
		case 3:
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
		case 4:
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
		{"Rename", ""}, {"Remove", ""}, {"← Back", ""},
	})
	if !ok || i == 2 {
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

func (s *menuSession) entryAdd() {
	name, ok := runInput("Name", "")
	if !ok || name == "" {
		return
	}
	link, ok := runInput("Share link (vless/vmess/trojan/ss)", "")
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
