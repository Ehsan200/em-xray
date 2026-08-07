package xray

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestBuildProbeConfig(t *testing.T) {
	items := []ProbeItem{
		{Name: "a", Outbound: `{"protocol":"freedom"}`},
		{Name: "b", Outbound: `{"protocol":"freedom","streamSettings":{"sockopt":{"dialerProxy":"master-dialer"}}}`},
	}
	raw, err := BuildProbeConfig(items, []int{31001, 31002})
	if err != nil {
		t.Fatal(err)
	}
	var cfg struct {
		Inbounds []struct {
			Tag      string `json:"tag"`
			Listen   string `json:"listen"`
			Port     int    `json:"port"`
			Protocol string `json:"protocol"`
		} `json:"inbounds"`
		Outbounds []map[string]any `json:"outbounds"`
		Routing   struct {
			Rules []struct {
				InboundTag  []string `json:"inboundTag"`
				OutboundTag string   `json:"outboundTag"`
			} `json:"rules"`
		} `json:"routing"`
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatal(err)
	}
	// Outbounds carry a leading blackhole (xray's default handler) on top of the
	// per-item ones, so a routing miss fails instead of being attributed to
	// whichever node happened to be first.
	if len(cfg.Inbounds) != 2 || len(cfg.Outbounds) != 3 || len(cfg.Routing.Rules) != 2 {
		t.Fatalf("want 2 inbounds / 3 outbounds / 2 rules, got %d / %d / %d",
			len(cfg.Inbounds), len(cfg.Outbounds), len(cfg.Routing.Rules))
	}
	if cfg.Outbounds[0]["tag"] != "block" || cfg.Outbounds[0]["protocol"] != "blackhole" {
		t.Fatalf("first probe outbound must be the blackhole default, got %v", cfg.Outbounds[0])
	}
	itemOutbounds := cfg.Outbounds[1:]
	for i, in := range cfg.Inbounds {
		if in.Listen != "127.0.0.1" {
			t.Errorf("inbound %d listens on %q, want loopback only", i, in.Listen)
		}
		if in.Protocol != "socks" {
			t.Errorf("inbound %d protocol %q, want socks", i, in.Protocol)
		}
	}
	if cfg.Inbounds[0].Port != 31001 || cfg.Inbounds[1].Port != 31002 {
		t.Errorf("ports not carried through: %d, %d", cfg.Inbounds[0].Port, cfg.Inbounds[1].Port)
	}
	// Each inbound must route to its OWN outbound, else results get crossed.
	for i, r := range cfg.Routing.Rules {
		wantIn, wantOut := cfg.Inbounds[i].Tag, itemOutbounds[i]["tag"]
		if len(r.InboundTag) != 1 || r.InboundTag[0] != wantIn || r.OutboundTag != wantOut {
			t.Errorf("rule %d: %v → %q, want [%s] → %q", i, r.InboundTag, r.OutboundTag, wantIn, wantOut)
		}
	}
	// The master dialerProxy hop is a live-config property, not part of the
	// server being probed.
	if strings.Contains(string(raw), "dialerProxy") {
		t.Errorf("probe config kept a dialerProxy:\n%s", raw)
	}
}

func TestBuildProbeConfigRejectsMismatch(t *testing.T) {
	if _, err := BuildProbeConfig([]ProbeItem{{Outbound: `{}`}}, []int{1, 2}); err == nil {
		t.Fatal("want error when ports and items disagree")
	}
}

func TestProbeOutboundsFlagsBadJSON(t *testing.T) {
	items := []ProbeItem{{Name: "broken", Outbound: `{nope`}}
	got := ProbeOutbounds(context.Background(), items, ProbeOptions{Bin: "/nonexistent/xray"})
	if len(got) != 1 {
		t.Fatalf("want 1 result, got %d", len(got))
	}
	if got[0].Err == nil || !strings.Contains(got[0].Err.Error(), "bad outbound json") {
		t.Fatalf("want a json error, got %v", got[0].Err)
	}
	if got[0].Name != "broken" {
		t.Errorf("identity not echoed back: %q", got[0].Name)
	}
}

func TestProbeOutboundsWithoutBinary(t *testing.T) {
	got := ProbeOutbounds(context.Background(), []ProbeItem{{Outbound: `{"protocol":"freedom"}`}}, ProbeOptions{})
	if len(got) != 1 || got[0].Err == nil {
		t.Fatalf("want an error result without a binary, got %+v", got)
	}
}

func TestFreeLoopbackPortsAreDistinct(t *testing.T) {
	ports, err := freeLoopbackPorts(5)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[int]bool{}
	for _, p := range ports {
		if p == 0 || seen[p] {
			t.Fatalf("bad port set %v", ports)
		}
		seen[p] = true
	}
}

func TestSetNodeLatency(t *testing.T) {
	s := memStore(t)
	sub := &Subscription{Name: "pool", URL: "http://x", Enabled: true}
	if err := s.CreateSubscription(sub); err != nil {
		t.Fatal(err)
	}
	n := mkNode(t, "a", `{"protocol":"vless","settings":{"address":"1.2.3.4","port":443}}`)
	if err := s.ReplaceNodes(sub.ID, []SubNode{n}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetNodeLatency(sub.ID, n.Fingerprint, 137); err != nil {
		t.Fatal(err)
	}
	nodes, err := s.NodesForSub(sub.ID)
	if err != nil {
		t.Fatal(err)
	}
	if nodes[0].LastLatencyMs != 137 {
		t.Fatalf("latency = %d, want 137", nodes[0].LastLatencyMs)
	}
	// A failed re-test clears the stale figure rather than keeping a lie.
	if err := s.SetNodeLatency(sub.ID, n.Fingerprint, 0); err != nil {
		t.Fatal(err)
	}
	nodes, _ = s.NodesForSub(sub.ID)
	if nodes[0].LastLatencyMs != 0 {
		t.Fatalf("latency = %d, want it cleared", nodes[0].LastLatencyMs)
	}
	// A refresh replaces the volatile rows; the measurement is keyed by
	// fingerprint, so re-recording after a refresh still lands.
	if err := s.ReplaceNodes(sub.ID, []SubNode{mkNode(t, "a", n.Outbound)}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetNodeLatency(sub.ID, n.Fingerprint, 42); err != nil {
		t.Fatal(err)
	}
	nodes, _ = s.NodesForSub(sub.ID)
	if nodes[0].LastLatencyMs != 42 {
		t.Fatalf("latency after refresh = %d, want 42", nodes[0].LastLatencyMs)
	}
}
