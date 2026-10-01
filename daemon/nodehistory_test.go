package daemon

import (
	"testing"
	"time"

	"github.com/ehsan200/em-xray/core/xray"
)

func histPool(picked []string, keys ...string) []xray.Slot {
	sl := parkSlot(keys...)
	sl[0].Key = "xraysub:s"
	sl[0].Picked = picked
	return sl
}

func TestNodeHistoryFlipsAndPickLoss(t *testing.T) {
	now := time.Unix(1000, 0)
	h := newNodeHistory()
	h.now = func() time.Time { return now }
	slots := histPool([]string{"a", "b", "c"}, "a", "b", "c", "d")
	rounds := []map[string]bool{
		{"a": true, "b": true, "c": true, "d": false},
		{"a": false, "b": false, "c": false, "d": true}, // whole pick dead
		{"a": false, "b": false, "c": false, "d": true}, // still the same loss
		{"a": true, "b": true, "c": false, "d": false},
		{"a": false, "b": false, "c": false, "d": true}, // second loss
	}
	for _, r := range rounds {
		h.record(slots, health(r), nil)
		now = now.Add(nodeHealthPollInterval)
	}
	ps := h.snapshot(0)
	if len(ps) != 1 {
		t.Fatalf("pools = %d, want 1", len(ps))
	}
	p := ps[0]
	if p.Rounds != 5 || p.PickLoss != 2 || p.LossSec != 30 {
		t.Fatalf("rounds=%d pickLoss=%d lossSec=%d, want 5/2/30", p.Rounds, p.PickLoss, p.LossSec)
	}
	by := map[string]NodeHealth{}
	for _, n := range p.Nodes {
		by[n.Key] = n
	}
	if n := by["a"]; n.Flips != 3 || n.UptimePct != 40 || n.Role != "active" {
		t.Fatalf("a = %+v, want 3 flips 40%% active", n)
	}
	if n := by["c"]; n.Flips != 1 || n.Role != "spare" {
		t.Fatalf("c = %+v, want 1 flip spare", n)
	}
	if n := by["d"]; n.Role != "idle" || n.Flips != 3 {
		t.Fatalf("d = %+v, want idle 3 flips", n)
	}
	if p.ChurnPct != 100 {
		t.Fatalf("churn = %d, want 100", p.ChurnPct)
	}
	if p.Nodes[0].Role != "active" || p.Nodes[len(p.Nodes)-1].Role != "idle" {
		t.Fatalf("order = %+v, want active first, idle last", p.Nodes)
	}
}

// A round with nothing alive anywhere is the uplink: no flips, no pick loss.
func TestNodeHistoryUplinkDownIgnored(t *testing.T) {
	now := time.Unix(1000, 0)
	h := newNodeHistory()
	h.now = func() time.Time { return now }
	slots := histPool([]string{"a", "b"}, "a", "b")
	for _, r := range []map[string]bool{
		{"a": true, "b": true},
		{"a": false, "b": false},
		{"a": true, "b": true},
	} {
		h.record(slots, health(r), nil)
		now = now.Add(nodeHealthPollInterval)
	}
	p := h.snapshot(0)[0]
	if p.PickLoss != 0 {
		t.Fatalf("pick loss = %d on an uplink outage", p.PickLoss)
	}
	for _, n := range p.Nodes {
		if n.Flips != 0 || n.UptimePct != 100 || n.States[1] != hsUplinkDown {
			t.Fatalf("%s = %+v, want no flips, 100%%, uplink-down middle", n.Key, n)
		}
	}
}

// A member the parker took out stays on its pool's strip as parked; one that
// left the pool otherwise shows as gone.
func TestNodeHistoryParkedAndGone(t *testing.T) {
	now := time.Unix(1000, 0)
	h := newNodeHistory()
	h.now = func() time.Time { return now }
	h.record(histPool(nil, "a", "b", "c"), health(map[string]bool{"a": true, "b": false, "c": true}), nil)
	now = now.Add(nodeHealthPollInterval)
	h.record(histPool(nil, "a"), health(map[string]bool{"a": true}), map[string]time.Time{"b": now.Add(time.Hour)})
	by := map[string]NodeHealth{}
	for _, n := range h.snapshot(0)[0].Nodes {
		by[n.Key] = n
	}
	if n := by["b"]; n.Role != "parked" || n.States[1] != hsParked {
		t.Fatalf("b = %+v, want parked", n)
	}
	if n := by["c"]; n.Role != "gone" || n.States[1] != hsNone {
		t.Fatalf("c = %+v, want gone", n)
	}
}

func TestNodeHistoryWindowAndPoolLifecycle(t *testing.T) {
	now := time.Unix(1000, 0)
	h := newNodeHistory()
	h.now = func() time.Time { return now }
	slots := histPool(nil, "a")
	for i := 0; i < int(nodeHistoryWindow/nodeHealthPollInterval)+20; i++ {
		h.record(slots, health(map[string]bool{"a": true}), nil)
		now = now.Add(nodeHealthPollInterval)
	}
	now = now.Add(-nodeHealthPollInterval)
	if got, max := h.snapshot(0)[0].Rounds, int(nodeHistoryWindow/nodeHealthPollInterval)+1; got > max {
		t.Fatalf("kept %d rounds, want ≤ %d", got, max)
	}
	if got := h.snapshot(time.Minute)[0].Rounds; got != 7 {
		t.Fatalf("1m window = %d rounds, want 7", got)
	}
	// Pool no longer loaded → forgotten.
	h.record(nil, nil, nil)
	if ps := h.snapshot(0); len(ps) != 0 {
		t.Fatalf("pools = %d after unload, want 0", len(ps))
	}
}

func TestNodeHistoryRename(t *testing.T) {
	h := newNodeHistory()
	h.record(histPool(nil, "xray-old"), health(map[string]bool{"xray-old": true}), nil)
	h.rename(func(k string) string {
		if k == "xraysub:s" {
			return "xraysub:t"
		}
		return k
	}, "xray-old", "xray-new")
	p := h.snapshot(0)[0]
	if p.Key != "xraysub:t" || p.Nodes[0].Key != "xray-new" {
		t.Fatalf("after rename: key=%s node=%s", p.Key, p.Nodes[0].Key)
	}
}
