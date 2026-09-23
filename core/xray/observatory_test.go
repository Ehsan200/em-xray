package xray

import "testing"

// Real `xray api bi` output for a leastLoad balancer with expected=2.
func TestParseBalancerSelects(t *testing.T) {
	text := "  - Selecting Override:\n    1                 \n  - Selects:\n    1   slot0-out-xray-good2\n    2   slot0-out-xray-good1\n"
	got := ParseBalancerSelects(text)["slot0-bal"]
	if len(got) != 2 || got[0] != "slot0-out-xray-good2" || got[1] != "slot0-out-xray-good1" {
		t.Fatalf("selects = %v", got)
	}
	if w := ParseBalancerWinners(text)["slot0-bal"]; w != "slot0-out-xray-good2" {
		t.Fatalf("winner = %q", w)
	}
}

func TestParseBalancerWinners(t *testing.T) {
	// Tolerant of exact bi text format — keys off the slotN-out- token.
	text := `Balancer: slot0-bal
  Strategy: leastPing
  Selects:
    - slot0-out-f03e9fda84be4072
Balancer: slot1-bal
  Selects:
    - slot1-out-xray-node1
`
	w := ParseBalancerWinners(text)
	if w["slot0-bal"] != "slot0-out-f03e9fda84be4072" {
		t.Errorf("slot0 winner = %q", w["slot0-bal"])
	}
	if w["slot1-bal"] != "slot1-out-xray-node1" {
		t.Errorf("slot1 winner = %q", w["slot1-bal"])
	}
}

func TestParseBalancerWinnersFirstWins(t *testing.T) {
	// If several members list under one slot, the first is taken as winner.
	text := "slot0-out-aaa\nslot0-out-bbb\n"
	w := ParseBalancerWinners(text)
	if w["slot0-bal"] != "slot0-out-aaa" {
		t.Errorf("expected first token, got %q", w["slot0-bal"])
	}
	if len(w) != 1 {
		t.Errorf("expected 1 balancer, got %d", len(w))
	}
}

func TestParseBalancerWinnersEmpty(t *testing.T) {
	if len(ParseBalancerWinners("no tags here")) != 0 {
		t.Error("expected no winners")
	}
}
