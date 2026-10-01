package xray

import (
	"reflect"
	"testing"
)

func TestOutboundEndpoint(t *testing.T) {
	for _, tc := range []struct {
		ob   string
		host string
		port int
	}{
		{`{"protocol":"vless","settings":{"vnext":[{"address":"a.example","port":443}]}}`, "a.example", 443},
		{`{"protocol":"trojan","settings":{"servers":[{"address":"1.2.3.4","port":8443}]}}`, "1.2.3.4", 8443},
		{`{"protocol":"vless","settings":{"address":"flat.example","port":"2053","id":"x"}}`, "flat.example", 2053},
		{`{"protocol":"hysteria","settings":{"version":2,"address":"h.example","port":443}}`, "h.example", 443},
		{`{"protocol":"wireguard","settings":{"peers":[{"endpoint":"162.159.192.1:2408"}]}}`, "162.159.192.1", 2408},
		{`{"protocol":"wireguard","settings":{"peers":[{"endpoint":"[2606:4700::1]:2408"}]}}`, "2606:4700::1", 2408},
	} {
		h, p, ok := OutboundEndpoint(tc.ob)
		if !ok || h != tc.host || p != tc.port {
			t.Errorf("%s → %q %d %v, want %q %d", tc.ob, h, p, ok, tc.host, tc.port)
		}
	}
	if _, _, ok := OutboundEndpoint(`{"protocol":"freedom"}`); ok {
		t.Error("freedom has no endpoint")
	}
}

func TestParseStrategy(t *testing.T) {
	for in, want := range map[string]string{"": "auto", "Auto": "auto", " agile ": "agile", "stable": "stable"} {
		if got, err := ParseStrategy(in); err != nil || got != want {
			t.Errorf("ParseStrategy(%q) = %q %v, want %q", in, got, err, want)
		}
	}
	if _, err := ParseStrategy("fast"); err == nil {
		t.Error("unknown strategy accepted")
	}
	if got := (Subscription{Strategy: "bogus"}).EffectiveStrategy(); got != StrategyAuto {
		t.Errorf("bogus strategy → %q, want auto", got)
	}
}

// An agile slot's balancer spreads over Active members; the next is fallback.
func TestGenerateAgileActiveCount(t *testing.T) {
	entries := []XrayEntry{{Name: "M", Enabled: true, Dialer: "xraysub:s", Outbound: `{"protocol":"vless","settings":{"vnext":[]}}`}}
	sock := `{"protocol":"socks","settings":{"servers":[{"address":"1.2.3.4","port":1}]}}`
	sl := Slot{Master: "M", Strategy: StrategyAgile, Active: 3, Picked: []string{"a", "b", "c", "d"}}
	for _, k := range []string{"a", "b", "c", "d", "e"} {
		sl.Members = append(sl.Members, SlotMember{Key: k, Outbound: sock})
	}
	b := dig(t, om(t, mustGen(t, entries, []Slot{sl})), "routing", "balancers", 0)
	if got := dig(t, b, "selector"); !reflect.DeepEqual(got, []any{"slot0-out-a", "slot0-out-b", "slot0-out-c"}) {
		t.Errorf("agile selector = %v, want the 3 active", got)
	}
	if got := dig(t, b, "fallbackTag"); got != "slot0-out-d" {
		t.Errorf("agile fallbackTag = %v, want spare d", got)
	}
	sl.Active = 0
	if sl.ActiveCount() != SlotBalancerExpected {
		t.Fatalf("default ActiveCount = %d", sl.ActiveCount())
	}
}

func TestAutoTuningSetting(t *testing.T) {
	s := memStore(t)
	if got := s.AutoTuning(); got != DefaultAutoTuning {
		t.Fatalf("unset = %+v, want defaults", got)
	}
	want := AutoTuning{WindowMin: 15, Flips: 3, FlappingPct: 40, PickLoss: 0, CalmMin: 60}
	if err := s.SetAutoTuning(want); err != nil {
		t.Fatal(err)
	}
	if got := s.AutoTuning(); got != want {
		t.Fatalf("stored = %+v, want %+v", got, want)
	}
	for _, bad := range []AutoTuning{
		{WindowMin: 1, Flips: 2, FlappingPct: 30, CalmMin: 30},
		{WindowMin: 31, Flips: 2, FlappingPct: 30, CalmMin: 30},
		{WindowMin: 10, Flips: 0, FlappingPct: 30, CalmMin: 30},
		{WindowMin: 10, Flips: 2, FlappingPct: 0, CalmMin: 30},
		{WindowMin: 10, Flips: 2, FlappingPct: 30, CalmMin: 0},
	} {
		if err := s.SetAutoTuning(bad); err == nil {
			t.Errorf("accepted %+v", bad)
		}
	}
}

// Pins are durable, enable the node, load it outside the cap; disabling unpins.
func TestNodePins(t *testing.T) {
	s := memStore(t)
	sub := &Subscription{Name: "p", URL: "http://x", Enabled: true, NodeCap: 2}
	if err := s.CreateSubscription(sub); err != nil {
		t.Fatal(err)
	}
	var nodes []SubNode
	for _, a := range []string{"1.1.1.1", "2.2.2.2", "3.3.3.3", "4.4.4.4"} {
		nodes = append(nodes, mkNode(t, a, `{"protocol":"socks","settings":{"servers":[{"address":"`+a+`","port":1}]}}`))
	}
	if err := s.ReplaceNodes(sub.ID, nodes); err != nil {
		t.Fatal(err)
	}
	fp := func(i int) string { return nodes[i].Fingerprint }
	active := func() map[string]bool {
		ns, _ := s.ActiveNodes(sub.ID)
		out := map[string]bool{}
		for _, n := range ns {
			out[n.Fingerprint] = true
		}
		return out
	}
	// A disabled node beyond the cap: pinning enables and loads it.
	if err := s.SetNodeDisabled(sub.ID, fp(3), true); err != nil {
		t.Fatal(err)
	}
	if err := s.SetNodePinned(sub.ID, fp(3), true); err != nil {
		t.Fatal(err)
	}
	if err := s.SetNodePinned(sub.ID, fp(2), true); err != nil {
		t.Fatal(err)
	}
	if a := active(); !a[fp(3)] || !a[fp(2)] || !a[fp(0)] || !a[fp(1)] {
		t.Fatalf("active = %v, want both pins plus the cap's 2", a)
	}
	if off, _ := s.DisabledFingerprints(sub.ID); off[fp(3)] {
		t.Fatal("pinned node still disabled")
	}
	if pins, _ := s.PinnedFingerprints(sub.ID); !reflect.DeepEqual(pins, []string{fp(3), fp(2)}) {
		t.Fatalf("pins = %v, want in pin order", pins)
	}
	// Survives a refresh.
	if err := s.ReplaceNodes(sub.ID, nodes); err != nil {
		t.Fatal(err)
	}
	if pins, _ := s.PinnedFingerprints(sub.ID); len(pins) != 2 {
		t.Fatalf("pins after refresh = %v", pins)
	}
	// Disabling unpins.
	if err := s.SetNodeDisabled(sub.ID, fp(2), true); err != nil {
		t.Fatal(err)
	}
	if pins, _ := s.PinnedFingerprints(sub.ID); !reflect.DeepEqual(pins, []string{fp(3)}) {
		t.Fatalf("pins after disable = %v", pins)
	}
	if err := s.ClearPins(sub.ID); err != nil {
		t.Fatal(err)
	}
	if pins, _ := s.PinnedFingerprints(sub.ID); len(pins) != 0 {
		t.Fatalf("pins after clear = %v", pins)
	}
	if a := active(); a[fp(3)] {
		t.Fatal("unpinned node beyond the cap still loaded")
	}
}

// A manual slot's balancer spreads over its pins and falls back to the first.
func TestGenerateManualBalancer(t *testing.T) {
	entries := []XrayEntry{{Name: "M", Enabled: true, Dialer: "xraysub:s", Outbound: `{"protocol":"vless","settings":{"vnext":[]}}`}}
	sock := `{"protocol":"socks","settings":{"servers":[{"address":"1.2.3.4","port":1}]}}`
	sl := Slot{Master: "M", Strategy: StrategyManual, Pinned: []string{"c", "a"}, Picked: []string{"c", "a"}, Active: 2}
	for _, k := range []string{"a", "b", "c"} {
		sl.Members = append(sl.Members, SlotMember{Key: k, Outbound: sock})
	}
	b := dig(t, om(t, mustGen(t, entries, []Slot{sl})), "routing", "balancers", 0)
	if got := dig(t, b, "selector"); !reflect.DeepEqual(got, []any{"slot0-out-c", "slot0-out-a"}) {
		t.Errorf("manual selector = %v, want the pins", got)
	}
	if got := dig(t, b, "fallbackTag"); got != "slot0-out-c" {
		t.Errorf("manual fallbackTag = %v, want the first pin", got)
	}
}
