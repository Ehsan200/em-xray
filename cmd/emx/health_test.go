package main

import (
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
	emxv1 "github.com/ehsan200/em-xray/api/emxv1"
)

func TestMergeStatesKeepsFlaps(t *testing.T) {
	in := []byte{hAlive, hAlive, hAlive, hDead, hDead, hDead, hParked, hUnknown}
	got := mergeStates(in, 4)
	want := []byte{hAlive, hMixed, hDead, hParked}
	if string(got) != string(want) {
		t.Fatalf("merge = %v, want %v", got, want)
	}
	if got := mergeStates(in, 20); len(got) != len(in) {
		t.Fatalf("short input merged: %v", got)
	}
}

func TestPoolMatches(t *testing.T) {
	p := &emxv1.PoolHealthPool{Masters: []string{"m1", "m2"}, Refs: "xray:e,xraysub:s"}
	for name, want := range map[string]bool{"": true, "m2": true, "s": true, "e": true, "x": false} {
		if got := poolMatches(p, name); got != want {
			t.Errorf("poolMatches(%q) = %v, want %v", name, got, want)
		}
	}
	if !poolUsesSub(p, "s") || poolUsesSub(p, "e") {
		t.Errorf("poolUsesSub wrong")
	}
}

func TestRenderHealthFitsWidth(t *testing.T) {
	states := make([]byte, 180)
	for i := range states {
		states[i] = hAlive + byte(i/7%2)
	}
	p := &emxv1.PoolHealthPool{Masters: []string{"m"}, Refs: "xraysub:s", Rounds: 180, PollSec: 10, Nodes: []*emxv1.PoolHealthNode{
		{Name: "🇩🇪 a very long node name that overflows", Role: "active", States: states, UptimePct: 50, Flips: 25, AvgRttMs: 120},
		{Name: "b", Role: "parked", States: states[:3]},
	}}
	out := renderHealth([]*emxv1.PoolHealthPool{p}, 80)
	for _, line := range strings.Split(out, "\n") {
		if w := lipgloss.Width(line); w > 80 && !strings.Contains(line, "uplink down") {
			t.Errorf("line %d wide: %q", w, line)
		}
	}
	if !strings.Contains(out, "parked") || !strings.Contains(out, "25 flips") {
		t.Errorf("missing fields:\n%s", out)
	}
}
