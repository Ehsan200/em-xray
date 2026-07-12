package xray

import "testing"

func memStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// mkNode builds a SubNode from an outbound JSON, computing its fingerprint.
func mkNode(t *testing.T, name, outbound string) SubNode {
	t.Helper()
	fp, err := Fingerprint([]byte(outbound))
	if err != nil {
		t.Fatalf("fingerprint %q: %v", outbound, err)
	}
	return SubNode{Name: name, Fingerprint: fp, Outbound: outbound}
}

func TestFingerprintStability(t *testing.T) {
	// Differ only by tag + display name — same identity.
	a := `{"tag":"out-a","protocol":"vless","settings":{"address":"1.2.3.4","port":443}}`
	b := `{"tag":"different","protocol":"vless","settings":{"port":443,"address":"1.2.3.4"}}`
	fa, _ := Fingerprint([]byte(a))
	fb, _ := Fingerprint([]byte(b))
	if fa != fb {
		t.Errorf("tag/key-order should not affect fingerprint: %s != %s", fa, fb)
	}
	if len(fa) != 16 {
		t.Errorf("fingerprint len = %d, want 16", len(fa))
	}
	// Different address — different identity.
	c := `{"protocol":"vless","settings":{"address":"9.9.9.9","port":443}}`
	fc, _ := Fingerprint([]byte(c))
	if fc == fa {
		t.Errorf("distinct content must differ: %s", fc)
	}
}

func TestReplaceNodesAndCap(t *testing.T) {
	s := memStore(t)
	sub := &Subscription{Name: "s1", URL: "https://x", NodeCap: 2}
	if err := s.CreateSubscription(sub); err != nil {
		t.Fatal(err)
	}
	nodes := []SubNode{
		mkNode(t, "n1", `{"protocol":"vless","settings":{"port":1}}`),
		mkNode(t, "n2", `{"protocol":"vless","settings":{"port":2}}`),
		mkNode(t, "n3", `{"protocol":"vless","settings":{"port":3}}`),
	}
	if err := s.ReplaceNodes(sub.ID, nodes); err != nil {
		t.Fatal(err)
	}
	active, err := s.ActiveNodes(sub.ID)
	if err != nil {
		t.Fatal(err)
	}
	// Cap 2 => first two in insertion order active.
	if len(active) != 2 {
		t.Fatalf("active = %d, want 2", len(active))
	}
	if active[0].Name != "n1" || active[1].Name != "n2" {
		t.Errorf("active order = %s,%s; want n1,n2", active[0].Name, active[1].Name)
	}
}

func TestOverrideSkipsCap(t *testing.T) {
	s := memStore(t)
	sub := &Subscription{Name: "s1", URL: "https://x", NodeCap: 2}
	if err := s.CreateSubscription(sub); err != nil {
		t.Fatal(err)
	}
	nodes := []SubNode{
		mkNode(t, "n1", `{"protocol":"vless","settings":{"port":1}}`),
		mkNode(t, "n2", `{"protocol":"vless","settings":{"port":2}}`),
		mkNode(t, "n3", `{"protocol":"vless","settings":{"port":3}}`),
	}
	if err := s.ReplaceNodes(sub.ID, nodes); err != nil {
		t.Fatal(err)
	}
	// Disable n1: it must not be active AND must not consume a cap slot, so n2+n3 fill cap.
	if err := s.SetNodeDisabled(sub.ID, nodes[0].Fingerprint, true); err != nil {
		t.Fatal(err)
	}
	active, _ := s.ActiveNodes(sub.ID)
	if len(active) != 2 {
		t.Fatalf("active = %d, want 2", len(active))
	}
	if active[0].Name != "n2" || active[1].Name != "n3" {
		t.Errorf("active = %s,%s; want n2,n3 (disabled n1 must not consume cap)", active[0].Name, active[1].Name)
	}
}

func TestOverrideSurvivesReplace(t *testing.T) {
	s := memStore(t)
	sub := &Subscription{Name: "s1", URL: "https://x", NodeCap: 30}
	if err := s.CreateSubscription(sub); err != nil {
		t.Fatal(err)
	}
	n1 := mkNode(t, "n1", `{"protocol":"vless","settings":{"port":1}}`)
	if err := s.ReplaceNodes(sub.ID, []SubNode{n1}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetNodeDisabled(sub.ID, n1.Fingerprint, true); err != nil {
		t.Fatal(err)
	}
	// Refresh: n1 returns (same fingerprint) at a new position, plus a new node.
	n0 := mkNode(t, "n0", `{"protocol":"vless","settings":{"port":9}}`)
	n1b := mkNode(t, "n1-renamed", `{"protocol":"vless","settings":{"port":1}}`)
	if n1b.Fingerprint != n1.Fingerprint {
		t.Fatal("precondition: same outbound must share fingerprint")
	}
	if err := s.ReplaceNodes(sub.ID, []SubNode{n0, n1b}); err != nil {
		t.Fatal(err)
	}
	active, _ := s.ActiveNodes(sub.ID)
	// Only n0 active; n1's durable override outlived the volatile rows.
	if len(active) != 1 || active[0].Name != "n0" {
		t.Errorf("active = %v, want [n0] (override must survive ReplaceNodes)", names(active))
	}
}

func TestDisabledSubAllInactive(t *testing.T) {
	s := memStore(t)
	sub := &Subscription{Name: "s1", URL: "https://x", Enabled: true}
	if err := s.CreateSubscription(sub); err != nil {
		t.Fatal(err)
	}
	if err := s.ReplaceNodes(sub.ID, []SubNode{
		mkNode(t, "n1", `{"protocol":"vless","settings":{"port":1}}`),
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetSubEnabled(sub.ID, false); err != nil {
		t.Fatal(err)
	}
	active, _ := s.ActiveNodes(sub.ID)
	if len(active) != 0 {
		t.Errorf("disabled sub should have 0 active, got %d", len(active))
	}
}

func TestValidName(t *testing.T) {
	if _, err := ValidName("  "); err == nil {
		t.Error("empty name should error")
	}
	got, err := ValidName("  My  Sub  ")
	if err != nil || got != "My Sub" {
		t.Errorf("normalize = %q, %v; want \"My Sub\", nil", got, err)
	}
	if _, err := ValidName("bad/name"); err == nil {
		t.Error("slash should be rejected")
	}
}

func names(ns []SubNode) []string {
	out := make([]string, len(ns))
	for i, n := range ns {
		out[i] = n.Name
	}
	return out
}
