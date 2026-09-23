package xray

import (
	"strings"
	"testing"
)

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
		{Key: "abc123", Outbound: `{"protocol":"socks","settings":{"servers":[{"address":"1.2.3.4","port":1080}]}}`},
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
	if dig(t, m, "burstObservatory", "pingConfig", "interval") != DefaultProbeInterval {
		t.Error("burst observatory missing")
	}

	// api routing rule must be FIRST.
	rules := dig(t, m, "routing", "rules").([]any)
	if dig(t, rules, 0, "inboundTag").([]any)[0] != "api" {
		t.Errorf("api rule must be first, got %v", rules[0])
	}

	// balancer over slot0-out- prefix.
	bals := dig(t, m, "routing", "balancers").([]any)
	if dig(t, bals, 0, "tag") != "slot0-bal" {
		t.Errorf("balancer tag = %v", dig(t, bals, 0, "tag"))
	}
	if dig(t, bals, 0, "selector").([]any)[0] != "slot0-out-" {
		t.Errorf("balancer selector = %v", dig(t, bals, 0, "selector"))
	}
	// leastLoad over the best two, so one node dying never takes every
	// connection of the pool with it.
	if dig(t, bals, 0, "strategy", "type") != "leastLoad" ||
		dig(t, bals, 0, "strategy", "settings", "expected") != float64(SlotBalancerExpected) {
		t.Errorf("balancer strategy = %v", dig(t, bals, 0, "strategy"))
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
		{Master: "Mbeta", Index: 1, Members: []SlotMember{{Key: "k1", Outbound: `{"protocol":"freedom"}`}}},
		{Master: "Malpha", Index: 0, Members: []SlotMember{{Key: "k2", Outbound: `{"protocol":"freedom"}`}}},
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

// The api, stats and burst observatory are always emitted — even with no
// masters — so adding the first master is a live apply, not a restart.
func TestGenerateNoSlotsKeepsLiveSections(t *testing.T) {
	b, _ := Generate([]XrayEntry{{Name: "e", Enabled: true, Outbound: `{"protocol":"freedom"}`}}, nil, nil, GenOptions{})
	m := om(t, b)
	for _, k := range []string{"api", "stats", "burstObservatory"} {
		if _, ok := m[k]; !ok {
			t.Errorf("%s must always be present", k)
		}
	}
	svcs := dig(t, m, "api", "services").([]any)
	var hasStats, hasObs bool
	for _, s := range svcs {
		switch s {
		case "StatsService":
			hasStats = true
		case "ObservatoryService":
			hasObs = true
		}
	}
	if !hasStats || !hasObs {
		t.Errorf("services = %v; want StatsService and ObservatoryService", svcs)
	}
	if _, ok := m["routing"].(map[string]any)["balancers"]; ok {
		t.Error("no slots => no balancers")
	}
}

// Masters sharing a pool share one slot: one inbound, one balancer, one set of
// members (so one set of probes) — but each keeps its own dialer outbound.
func TestGenerateSharedSlot(t *testing.T) {
	entries := []XrayEntry{
		{Name: "A", Enabled: true, Dialer: "xraysub:s", Outbound: `{"protocol":"vless","settings":{"vnext":[]}}`},
		{Name: "B", Enabled: true, Dialer: "xraysub:s", Outbound: `{"protocol":"vless","settings":{"vnext":[]}}`},
	}
	slots := []Slot{{Master: "A", Aliases: []string{"B"}, Index: 0, Members: []SlotMember{
		{Key: "n1", Outbound: `{"protocol":"socks","settings":{"servers":[{"address":"1.2.3.4","port":1}]}}`},
	}}}
	m := om(t, mustGen(t, entries, slots))

	if n := len(dig(t, m, "routing", "balancers").([]any)); n != 1 {
		t.Fatalf("balancers = %d, want 1 shared", n)
	}
	var slotIns, members int
	for _, in := range dig(t, m, "inbounds").([]any) {
		if tag, _ := in.(map[string]any)["tag"].(string); strings.HasPrefix(tag, "slot") {
			slotIns++
		}
	}
	dialers := map[string]any{}
	proxies := map[string]any{}
	for _, o := range dig(t, m, "outbounds").([]any) {
		ob := o.(map[string]any)
		tag := ob["tag"].(string)
		switch {
		case strings.HasPrefix(tag, "slot0-out-"):
			members++
		case strings.HasPrefix(tag, "dialer-"):
			dialers[tag] = dig(t, ob, "settings", "servers", 0, "port")
		case tag == "out-A" || tag == "out-B":
			proxies[tag] = dig(t, ob, "streamSettings", "sockopt", "dialerProxy")
		}
	}
	if slotIns != 1 || members != 1 {
		t.Errorf("slot inbounds = %d, members = %d; want 1 and 1", slotIns, members)
	}
	port := float64(SlotPort(0))
	if len(dialers) != 2 || dialers["dialer-A"] != port || dialers["dialer-B"] != port {
		t.Errorf("each master needs its own dialer outbound into the shared slot: %v", dialers)
	}
	if proxies["out-A"] != "dialer-A" || proxies["out-B"] != "dialer-B" {
		t.Errorf("each master must keep its own dialerProxy: %v", proxies)
	}
}

// A master left without a slot (all slots used) must not dial off this box.
func TestGenerateSlotlessMasterBlocked(t *testing.T) {
	entries := []XrayEntry{{Name: "M", Enabled: true, Dialer: "xraysub:s", Outbound: `{"protocol":"vless","settings":{"vnext":[]}}`}}
	m := om(t, mustGen(t, entries, nil))
	for _, o := range dig(t, m, "outbounds").([]any) {
		ob := o.(map[string]any)
		if ob["tag"] == "out-M" {
			if got := dig(t, ob, "streamSettings", "sockopt", "dialerProxy"); got != "block" {
				t.Fatalf("slotless master dialerProxy = %v, want block", got)
			}
			return
		}
	}
	t.Fatal("out-M missing")
}

// The balancer falls back to the pool's first TUNNELLING member (so a fresh
// slot works before the first ping lands), and to block only when the pool is
// empty. A freedom member is never emitted: it would egress from this box.
func TestGenerateFallbackIsFirstTunnelMember(t *testing.T) {
	entries := []XrayEntry{{Name: "M", Enabled: true, Dialer: "xraysub:s", Outbound: `{"protocol":"vless","settings":{"vnext":[]}}`}}
	sock := `{"protocol":"socks","settings":{"servers":[{"address":"1.2.3.4","port":1}]}}`
	slots := []Slot{{Master: "M", Members: []SlotMember{
		{Key: "direct", Outbound: `{"protocol":"freedom"}`},
		{Key: "n1", Outbound: sock},
		{Key: "n2", Outbound: sock},
	}}}
	m := om(t, mustGen(t, entries, slots))
	if got := dig(t, m, "routing", "balancers", 0, "fallbackTag"); got != "slot0-out-n1" {
		t.Errorf("fallbackTag = %v, want first tunnelling member slot0-out-n1", got)
	}
	for _, o := range dig(t, m, "outbounds").([]any) {
		if o.(map[string]any)["tag"] == "slot0-out-direct" {
			t.Error("freedom member emitted — it would dial the master's server off this box")
		}
	}

	slots[0].Members = nil
	m = om(t, mustGen(t, entries, slots))
	if got := dig(t, m, "routing", "balancers", 0, "fallbackTag"); got != "block" {
		t.Errorf("empty pool fallbackTag = %v, want block", got)
	}
}

func TestCheckMemberOutbound(t *testing.T) {
	for _, ok := range []string{"vless", "vmess", "trojan", "shadowsocks", "socks", "http", "hysteria", "wireguard"} {
		if err := CheckMemberOutbound(`{"protocol":"` + ok + `"}`); err != nil {
			t.Errorf("%s rejected: %v", ok, err)
		}
	}
	for _, bad := range []string{"freedom", "blackhole", "dns", "loopback", ""} {
		if CheckMemberOutbound(`{"protocol":"`+bad+`"}`) == nil {
			t.Errorf("%q accepted as a pool member", bad)
		}
	}
}

func TestDialerGroupKeyOrderInsensitive(t *testing.T) {
	a, _ := ParseDialer("xraysub:s,xray:n")
	b, _ := ParseDialer("xray:n, xraysub:s, xray:n")
	c, _ := ParseDialer("xraysub:s")
	if DialerGroupKey(a) != DialerGroupKey(b) {
		t.Errorf("same refs in another order must share a key: %q vs %q", DialerGroupKey(a), DialerGroupKey(b))
	}
	if DialerGroupKey(a) == DialerGroupKey(c) {
		t.Error("different ref sets must not share a key")
	}
}
