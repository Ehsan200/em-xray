package xray

import "testing"

func TestParseDialer(t *testing.T) {
	refs, err := ParseDialer(" xray:A , xraysub:S ,proxy:P ")
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 3 || refs[0] != (DialerRef{RefXray, "A"}) || refs[1] != (DialerRef{RefXraySub, "S"}) || refs[2] != (DialerRef{RefProxy, "P"}) {
		t.Errorf("refs = %+v", refs)
	}
	if r, _ := ParseDialer(""); r != nil {
		t.Error("empty dialer should yield no refs")
	}
	for _, bad := range []string{"foo:x", "xray:", "justtext"} {
		if _, err := ParseDialer(bad); err == nil {
			t.Errorf("expected error for %q", bad)
		}
	}
}

func TestDetectDialerCycle(t *testing.T) {
	// A → xray:B, B → xray:A  ==> cycle.
	stored := map[string]string{"A": "xray:B", "B": "xray:A"}
	dialerOf := func(n string) (string, bool) { d, ok := stored[n]; return d, ok }

	if err := DetectDialerCycle("A", "xray:B", dialerOf); err == nil {
		t.Error("A→B→A should be detected as a cycle")
	}
	// Self reference.
	if err := DetectDialerCycle("X", "xray:X", func(string) (string, bool) { return "", false }); err == nil {
		t.Error("self-reference should be a cycle")
	}
	// xraysub is a leaf — no cycle even if named like the entry.
	if err := DetectDialerCycle("A", "xraysub:A", func(string) (string, bool) { return "", false }); err != nil {
		t.Errorf("xraysub ref must not be treated as a loop: %v", err)
	}
	// Acyclic chain A→B (B a leaf).
	if err := DetectDialerCycle("A", "xray:B", func(n string) (string, bool) { return "", false }); err != nil {
		t.Errorf("acyclic chain should pass: %v", err)
	}
}

func TestGenerateWithSlots(t *testing.T) {
	entries := []XrayEntry{
		{Name: "M", Enabled: true, Dialer: "xray:node1", Outbound: `{"protocol":"vless","settings":{"vnext":[]}}`},
		{Name: "node1", Enabled: true, Outbound: `{"protocol":"freedom","settings":{}}`},
	}
	slots := []Slot{{Master: "M", Members: []SlotMember{
		{Key: "abc123", Outbound: `{"protocol":"freedom","settings":{}}`},
	}}}
	b, err := Generate(entries, nil, slots, GenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	m := om(t, b)

	// api + observatory present.
	if dig(t, m, "api", "tag") != "api" {
		t.Error("api block missing")
	}
	if dig(t, m, "observatory", "probeInterval") != DefaultProbeInterval {
		t.Error("observatory missing")
	}

	// api routing rule must be FIRST.
	rules := dig(t, m, "routing", "rules").([]any)
	if dig(t, rules, 0, "inboundTag").([]any)[0] != "api" {
		t.Errorf("api rule must be first, got %v", rules[0])
	}

	// balancer over slot0-out- prefix, leastPing.
	bals := dig(t, m, "routing", "balancers").([]any)
	if dig(t, bals, 0, "tag") != "slot0-bal" {
		t.Errorf("balancer tag = %v", dig(t, bals, 0, "tag"))
	}
	if dig(t, bals, 0, "selector").([]any)[0] != "slot0-out-" {
		t.Errorf("balancer selector = %v", dig(t, bals, 0, "selector"))
	}

	// master outbound got the dialerProxy sockopt.
	outs := dig(t, m, "outbounds").([]any)
	var masterOut map[string]any
	var memberFound bool
	for _, o := range outs {
		om := o.(map[string]any)
		if om["tag"] == "out-M" {
			masterOut = om
		}
		if om["tag"] == "slot0-out-abc123" {
			memberFound = true
		}
	}
	if masterOut == nil {
		t.Fatal("master outbound out-M not found")
	}
	if dig(t, masterOut, "streamSettings", "sockopt", "dialerProxy") != "dialer-M" {
		t.Errorf("master dialerProxy not set: %v", masterOut["streamSettings"])
	}
	if !memberFound {
		t.Error("member outbound slot0-out-abc123 not found")
	}
}

func TestGenerateMultipleMasters(t *testing.T) {
	// Two independent masters => two slots (slot0, slot1), two balancers, each
	// with its own dialerProxy. Slot index is sorted-master-name order.
	entries := []XrayEntry{
		{Name: "Malpha", Enabled: true, Dialer: "xray:n", Outbound: `{"protocol":"freedom"}`},
		{Name: "Mbeta", Enabled: true, Dialer: "xray:n", Outbound: `{"protocol":"freedom"}`},
		{Name: "n", Enabled: true, Outbound: `{"protocol":"freedom","settings":{}}`},
	}
	slots := []Slot{
		{Master: "Mbeta", Members: []SlotMember{{Key: "k1", Outbound: `{"protocol":"freedom"}`}}},
		{Master: "Malpha", Members: []SlotMember{{Key: "k2", Outbound: `{"protocol":"freedom"}`}}},
	}
	m := om(t, mustGen(t, entries, slots))

	bals := dig(t, m, "routing", "balancers").([]any)
	if len(bals) != 2 {
		t.Fatalf("balancers = %d, want 2", len(bals))
	}
	// Malpha sorts first => slot0; Mbeta => slot1.
	outs := dig(t, m, "outbounds").([]any)
	proxies := map[string]string{}
	for _, o := range outs {
		om := o.(map[string]any)
		tag, _ := om["tag"].(string)
		if ss, ok := om["streamSettings"].(map[string]any); ok {
			if so, ok := ss["sockopt"].(map[string]any); ok {
				proxies[tag] = so["dialerProxy"].(string)
			}
		}
	}
	if proxies["out-Malpha"] != "dialer-Malpha" || proxies["out-Mbeta"] != "dialer-Mbeta" {
		t.Errorf("each master needs its own dialerProxy: %v", proxies)
	}
}

func mustGen(t *testing.T, entries []XrayEntry, slots []Slot) []byte {
	t.Helper()
	b, err := Generate(entries, nil, slots, GenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestGenerateNoSlotsNoApi(t *testing.T) {
	b, _ := Generate([]XrayEntry{{Name: "e", Enabled: true, Outbound: `{"protocol":"freedom"}`}}, nil, nil, GenOptions{})
	m := om(t, b)
	if _, ok := m["api"]; ok {
		t.Error("no slots => no api block")
	}
	if _, ok := m["observatory"]; ok {
		t.Error("no slots => no observatory")
	}
}
