package xray

import "testing"

func TestRenameDialerRef(t *testing.T) {
	got, changed := RenameDialerRef("xray:A,xraysub:S", RefXray, "A", "B")
	if !changed || got != "xray:B,xraysub:S" {
		t.Errorf("got %q changed=%v", got, changed)
	}
	// Wrong kind — no change.
	if _, changed := RenameDialerRef("xray:A", RefXraySub, "A", "B"); changed {
		t.Error("xraysub rename must not touch xray:A")
	}
	// Collision dedupes.
	got, changed = RenameDialerRef("xray:A,xray:B", RefXray, "A", "B")
	if !changed || got != "xray:B" {
		t.Errorf("collision should dedupe to xray:B, got %q", got)
	}
}

func TestRenameEntryCascade(t *testing.T) {
	s := memStore(t)
	if err := s.CreateEntry(&XrayEntry{Name: "node", Enabled: true, Outbound: `{"protocol":"freedom"}`}); err != nil {
		t.Fatal(err)
	}
	master := &XrayEntry{Name: "M", Enabled: true, Dialer: "xray:node", Outbound: `{"protocol":"freedom"}`}
	if err := s.CreateEntry(master); err != nil {
		t.Fatal(err)
	}
	node, _ := s.GetEntryByName("node")
	if err := s.RenameEntry(node.ID, "node2"); err != nil {
		t.Fatal(err)
	}
	// Master's dialer must now point at the new name.
	m, _ := s.GetEntryByName("M")
	if m.Dialer != "xray:node2" {
		t.Errorf("dialer not cascaded: %q", m.Dialer)
	}
	if _, err := s.GetEntryByName("node2"); err != nil {
		t.Errorf("renamed entry missing: %v", err)
	}
}

func TestRenameSubCascade(t *testing.T) {
	s := memStore(t)
	sub := &Subscription{Name: "mysub", URL: "https://x"}
	if err := s.CreateSubscription(sub); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateEntry(&XrayEntry{Name: "M", Enabled: true, Dialer: "xraysub:mysub", Outbound: `{"protocol":"freedom"}`}); err != nil {
		t.Fatal(err)
	}
	if err := s.RenameSubscription(sub.ID, "newsub"); err != nil {
		t.Fatal(err)
	}
	m, _ := s.GetEntryByName("M")
	if m.Dialer != "xraysub:newsub" {
		t.Errorf("sub rename not cascaded: %q", m.Dialer)
	}
}

func TestNamesExist(t *testing.T) {
	s := memStore(t)
	s.CreateEntry(&XrayEntry{Name: "here", Enabled: true, Outbound: `{"protocol":"freedom"}`})
	if m := s.NamesExist([]string{"here"}); len(m) != 0 {
		t.Errorf("existing name reported missing: %v", m)
	}
	if m := s.NamesExist([]string{"ghost"}); len(m) != 1 {
		t.Errorf("missing name not reported: %v", m)
	}
}

// Renaming an entry repoints every inbound routed to it (as a master or a
// plain xray target) and carries its traffic history to the new tag; things
// bound to other names are untouched.
func TestRenameEntryCascadesInboundsAndTraffic(t *testing.T) {
	s := memStore(t)
	for _, e := range []XrayEntry{
		{Name: "node", Enabled: true, Outbound: `{"protocol":"socks","settings":{"servers":[{"address":"1.2.3.4","port":1}]}}`},
		{Name: "M", Enabled: true, Dialer: "xray:node", Outbound: `{"protocol":"freedom"}`},
		{Name: "other", Enabled: true, Outbound: `{"protocol":"freedom"}`},
	} {
		e := e
		if err := s.CreateEntry(&e); err != nil {
			t.Fatal(err)
		}
	}
	for _, in := range []Inbound{
		{Name: "via-master", Protocol: "socks", Port: 20001, Target: "master:M"},
		{Name: "via-node", Protocol: "socks", Port: 20002, Target: "xray:node"},
		{Name: "via-other", Protocol: "socks", Port: 20003, Target: "xray:other"},
		{Name: "direct", Protocol: "socks", Port: 20004, Target: "direct"},
	} {
		in := in
		if err := s.CreateInbound(&in); err != nil {
			t.Fatal(err)
		}
	}
	// History under both tags: the new name's rows (a deleted entry that once
	// had it) are added to, not overwritten.
	must(t, s.AddTraffic(KindOutbound, OutboundTag("M"), "M", 3600, 10, 20))
	must(t, s.AddTraffic(KindOutbound, OutboundTag("M"), "M", 7200, 1, 2))
	must(t, s.AddTraffic(KindOutbound, OutboundTag("M2"), "M2", 3600, 100, 200))

	m, _ := s.GetEntryByName("M")
	must(t, s.RenameEntry(m.ID, "M2"))
	node, _ := s.GetEntryByName("node")
	must(t, s.RenameEntry(node.ID, "node2"))

	want := map[string]string{"via-master": "master:M2", "via-node": "xray:node2", "via-other": "xray:other", "direct": "direct"}
	ins, _ := s.ListInbounds()
	for _, in := range ins {
		if in.Target != want[in.Name] {
			t.Errorf("inbound %s target = %q, want %q", in.Name, in.Target, want[in.Name])
		}
	}
	m2, _ := s.GetEntryByName("M2")
	if m2.Dialer != "xray:node2" {
		t.Errorf("master dialer = %q, want xray:node2", m2.Dialer)
	}
	if up, down := s.TrafficTotalFor(KindOutbound, OutboundTag("M2")); up != 111 || down != 222 {
		t.Errorf("M2 lifetime = %d/%d, want 111/222", up, down)
	}
	if up, down := s.TrafficTotalFor(KindOutbound, OutboundTag("M")); up != 0 || down != 0 {
		t.Errorf("old tag kept traffic %d/%d", up, down)
	}
	bs, _ := s.TrafficBucketsSince(KindOutbound, OutboundTag("M2"), 0)
	got := map[int64]int64{}
	for _, b := range bs {
		got[b.HourUnix] = b.Up
	}
	if got[3600] != 110 || got[7200] != 1 || len(got) != 2 {
		t.Errorf("M2 buckets = %v, want 3600:110 7200:1", got)
	}
	totals, _ := s.TrafficTotals()
	for _, tt := range totals {
		if tt.Tag == OutboundTag("M2") && tt.Name != "M2" {
			t.Errorf("total name = %q, want M2", tt.Name)
		}
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
