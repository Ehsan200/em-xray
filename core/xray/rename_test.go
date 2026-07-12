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
