package daemon

import (
	"reflect"
	"testing"
	"time"

	"github.com/ehsan200/em-xray/core/xray"
)

func agileSlot(keys ...string) []xray.Slot {
	sl := pickSlot(keys...)
	sl[0].Strategy = xray.StrategyAgile
	return sl
}

// Agile spreads over every live member (fastest by the latest round, capped),
// spare next; no smoothing — one round flips the pick.
func TestPickAgileFollowsTheWave(t *testing.T) {
	p := newNodePicker()
	sl := agileSlot("a", "b", "c", "d", "e", "f", "g")
	p.observe(sl, tags(map[string]nodeStatus{
		"a": win(2, 0, 100), "b": win(2, 0, 200), "c": win(2, 0, 300),
		"d": win(2, 2, 0), "e": win(2, 2, 0), "f": win(2, 0, 50), "g": win(2, 0, 900),
	}))
	got, active := p.pickFor(sl[0], poolPick(xray.Slot{}), nil)
	// Top agileActiveMax alive by RTT (sorted by key), then the next alive.
	if want := []string{"a", "b", "c", "f", "g"}; !reflect.DeepEqual(got, want) || active != agileActiveMax {
		t.Fatalf("pick = %v active %d, want %v active %d", got, active, want, agileActiveMax)
	}

	// The wave turns: a b c f die, d e come up — the very next round follows.
	p.observe(sl, tags(map[string]nodeStatus{
		"a": win(2, 2, 0), "b": win(2, 2, 0), "c": win(2, 2, 0),
		"d": win(2, 0, 150), "e": win(2, 0, 120), "f": win(2, 2, 0), "g": win(2, 0, 900),
	}))
	prev := sl[0]
	prev.Picked, prev.Active = got, active
	got, active = p.pickFor(sl[0], poolPick(prev), nil)
	if len(got) < 3 || active != 3 || !reflect.DeepEqual(got[:3], []string{"d", "e", "g"}) {
		t.Fatalf("after the wave pick = %v active %d, want d e g active", got, active)
	}
	// The spare is the dead member with the best record, not a random one.
	if len(got) != 4 {
		t.Fatalf("no spare: %v", got)
	}
	if down := p.wentDown(sl[0]); !reflect.DeepEqual(down, []string{"a", "b", "c", "f"}) {
		t.Fatalf("wentDown = %v, want a b c f", down)
	}
}

// RTT noise alone doesn't swap an active member for a marginally faster one.
func TestPickAgileSticky(t *testing.T) {
	p := newNodePicker()
	sl := agileSlot("a", "b", "c", "d", "e")
	p.observe(sl, tags(map[string]nodeStatus{
		"a": win(2, 0, 100), "b": win(2, 0, 110), "c": win(2, 0, 120), "d": win(2, 0, 200), "e": win(2, 0, 190),
	}))
	prev := sl[0]
	prev.Picked, prev.Active = []string{"a", "b", "c", "d", "e"}, 4
	got, _ := p.pickFor(sl[0], poolPick(prev), nil)
	if !reflect.DeepEqual(got[:4], []string{"a", "b", "c", "d"}) {
		t.Fatalf("active d lost its place to marginally faster e: %v", got)
	}
}

// With nothing alive the agile pick falls back to the stable ranking.
func TestPickAgileNothingAlive(t *testing.T) {
	p := newNodePicker()
	sl := agileSlot("a", "b", "c")
	p.observe(sl, tags(map[string]nodeStatus{"a": win(2, 0, 100), "b": win(2, 0, 100), "c": win(2, 0, 100)}))
	// A round with every node dead is the uplink and ignored, so make one live
	// elsewhere: a second slot keeps the round meaningful.
	both := append(agileSlot("a", "b", "c"), xray.Slot{Index: 1, Members: []xray.SlotMember{{Key: "z"}}})
	h := tags(map[string]nodeStatus{"a": win(2, 2, 0), "b": win(2, 2, 0), "c": win(2, 2, 0)})
	h[xray.SlotMemberTag(1, "z")] = win(2, 0, 10)
	p.observe(both, h)
	got, active := p.pickFor(sl[0], poolPick(xray.Slot{}), nil)
	if len(got) == 0 || active != min(xray.SlotBalancerExpected, len(got)) {
		t.Fatalf("pick = %v active %d, want the stable fallback", got, active)
	}
}

// Agile pools never park, and a pool turning agile releases its parked nodes.
func TestAgileNeverParks(t *testing.T) {
	now := time.Unix(0, 0)
	p := newNodeParker()
	p.now = func() time.Time { return now }
	slots := parkSlot("good", "dead")
	slots[0].Strategy = xray.StrategyAgile
	h := health(map[string]bool{"good": true, "dead": false})
	p.observe(slots, h)
	now = now.Add(nodeDeadBeforePark * 2)
	if ev := p.observe(slots, h); len(ev) != 0 {
		t.Fatalf("agile pool parked: %+v", ev)
	}

	slots[0].Strategy = xray.StrategyStable
	p.observe(slots, h)
	now = now.Add(nodeDeadBeforePark)
	if ev := p.observe(slots, h); len(ev) != 1 {
		t.Fatalf("stable pool didn't park: %+v", ev)
	}
	if back := p.unpark([]string{"dead", "good"}); !reflect.DeepEqual(back, []string{"dead"}) {
		t.Fatalf("unpark = %v, want dead", back)
	}
	if len(p.parked()) != 0 {
		t.Fatalf("still parked: %v", p.parked())
	}
}

// A pool takes the most aggressive strategy of the subscriptions it draws on,
// and an agile pool keeps its parked nodes in.
func TestResolveAgilePool(t *testing.T) {
	store, sup := newTestSupervisor(t)
	mk := func(name, strategy string, addrs ...string) {
		sub := &xray.Subscription{Name: name, URL: "http://x/" + name, Enabled: true, Strategy: strategy}
		if err := store.CreateSubscription(sub); err != nil {
			t.Fatal(err)
		}
		var nodes []xray.SubNode
		for _, a := range addrs {
			nodes = append(nodes, xray.SubNode{Name: a, Fingerprint: "fp-" + a,
				Outbound: `{"protocol":"socks","settings":{"servers":[{"address":"` + a + `","port":1}]}}`})
		}
		if err := store.ReplaceNodes(sub.ID, nodes); err != nil {
			t.Fatal(err)
		}
	}
	mk("calm", "", "1.1.1.1", "1.1.1.2")
	mk("wild", xray.StrategyAgile, "2.2.2.1", "2.2.2.2")
	for _, e := range []xray.XrayEntry{
		{Name: "A", Enabled: true, Dialer: "xraysub:calm", Outbound: `{"protocol":"freedom"}`},
		{Name: "B", Enabled: true, Dialer: "xraysub:calm,xraysub:wild", Outbound: `{"protocol":"freedom"}`},
	} {
		e := e
		if err := store.CreateEntry(&e); err != nil {
			t.Fatal(err)
		}
	}
	// Park one node of each pool.
	sup.parker.nodes["fp-1.1.1.1"] = &parkState{parkedUntil: time.Now().Add(time.Hour)}
	sup.parker.nodes["fp-2.2.2.1"] = &parkState{parkedUntil: time.Now().Add(time.Hour)}

	entries, _ := store.ListEntries()
	slots, err := sup.resolveDialerSlots(entries)
	if err != nil {
		t.Fatal(err)
	}
	by := map[string]xray.Slot{}
	for _, sl := range slots {
		by[sl.Master] = sl
	}
	if a := by["A"]; a.Strategy != xray.StrategyStable || len(a.Members) != 1 {
		t.Errorf("A = %s with %d members, want stable with the parked node out", a.Strategy, len(a.Members))
	}
	if b := by["B"]; b.Strategy != xray.StrategyAgile || len(b.Members) != 4 {
		t.Errorf("B = %s with %d members, want agile with every node", b.Strategy, len(b.Members))
	}
}

// Manual wins over every other strategy; its pick is the pins present in the
// pool, it never parks, and with no pin present the pool runs stable.
func TestResolveManualPool(t *testing.T) {
	store, sup := newTestSupervisor(t)
	mk := func(name, strategy string, addrs ...string) *xray.Subscription {
		sub := &xray.Subscription{Name: name, URL: "http://x/" + name, Enabled: true, Strategy: strategy}
		if err := store.CreateSubscription(sub); err != nil {
			t.Fatal(err)
		}
		var nodes []xray.SubNode
		for _, a := range addrs {
			nodes = append(nodes, xray.SubNode{Name: a, Fingerprint: "fp-" + a,
				Outbound: `{"protocol":"socks","settings":{"servers":[{"address":"` + a + `","port":1}]}}`})
		}
		if err := store.ReplaceNodes(sub.ID, nodes); err != nil {
			t.Fatal(err)
		}
		return sub
	}
	man := mk("man", xray.StrategyManual, "1.1.1.1", "1.1.1.2", "1.1.1.3")
	mk("wild", xray.StrategyAgile, "2.2.2.1")
	if err := store.CreateEntry(&xray.XrayEntry{Name: "M", Enabled: true, Dialer: "xraysub:man,xraysub:wild", Outbound: `{"protocol":"freedom"}`}); err != nil {
		t.Fatal(err)
	}
	resolve := func() xray.Slot {
		entries, _ := store.ListEntries()
		slots, err := sup.resolveDialerSlots(entries)
		if err != nil || len(slots) != 1 {
			t.Fatalf("slots = %+v, %v", slots, err)
		}
		sup.loadedSlots = slots
		return slots[0]
	}
	if sl := resolve(); sl.Strategy != xray.StrategyStable {
		t.Fatalf("manual pool with no pins = %s, want stable", sl.Strategy)
	}
	sup.parker.nodes["fp-1.1.1.3"] = &parkState{parkedUntil: time.Now().Add(time.Hour)}
	for _, fp := range []string{"fp-1.1.1.3", "fp-1.1.1.1"} {
		if err := store.SetNodePinned(man.ID, fp, true); err != nil {
			t.Fatal(err)
		}
	}
	sl := resolve()
	if !sl.Manual() || sl.Auto {
		t.Fatalf("strategy = %s auto=%v, want manual over agile", sl.Strategy, sl.Auto)
	}
	if len(sl.Members) != 4 {
		t.Fatalf("members = %d, want all 4 (parked pin back)", len(sl.Members))
	}
	if !reflect.DeepEqual(sl.Picked, []string{"fp-1.1.1.3", "fp-1.1.1.1"}) || sl.ActiveCount() != 2 {
		t.Fatalf("pick = %v active %d, want the pins, all active", sl.Picked, sl.ActiveCount())
	}
	// Health never repicks a manual pool away from its pins.
	p := sup.picker
	p.observe([]xray.Slot{sl}, map[string]nodeStatus{xray.SlotMemberTag(sl.Index, "fp-1.1.1.3"): win(2, 2, 0), xray.SlotMemberTag(sl.Index, "fp-2.2.2.1"): win(2, 0, 10)})
	if got, _ := p.pickFor(sl, poolPick(sl), nil); !reflect.DeepEqual(got, sl.Pinned) {
		t.Fatalf("repick = %v, want the pins", got)
	}
}
