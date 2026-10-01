package daemon

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/ehsan200/em-xray/core/xray"
)

func nodesWithFlips(flips ...int) []NodeHealth {
	var out []NodeHealth
	for i, f := range flips {
		out = append(out, NodeHealth{Key: string(rune('a' + i)), UptimePct: 50, Flips: f})
	}
	return out
}

func TestFlappingJudgement(t *testing.T) {
	tu := xray.DefaultAutoTuning // 2 flips, 30%, 2 pick losses
	for _, tc := range []struct {
		name string
		p    PoolHealth
		want bool
	}{
		{"calm", PoolHealth{Nodes: nodesWithFlips(0, 0, 1, 0)}, false},
		{"one flaky node is not a wave", PoolHealth{Nodes: nodesWithFlips(9, 0, 0)}, false},
		{"share of nodes flapping", PoolHealth{Nodes: nodesWithFlips(2, 3, 0, 0, 0, 0)}, true},
		{"too small a share", PoolHealth{Nodes: nodesWithFlips(2, 3, 0, 0, 0, 0, 0, 0, 0, 0)}, false},
		{"whole pick lost twice", PoolHealth{PickLoss: 2, Nodes: nodesWithFlips(1, 1)}, true},
		{"unobserved nodes don't count", PoolHealth{Nodes: append(nodesWithFlips(2, 2), NodeHealth{UptimePct: -1}, NodeHealth{UptimePct: -1}, NodeHealth{UptimePct: -1}, NodeHealth{UptimePct: -1}, NodeHealth{UptimePct: -1})}, true},
	} {
		if got, why := flapping(tc.p, tu); got != tc.want {
			t.Errorf("%s: flapping = %v (%s), want %v", tc.name, got, why, tc.want)
		}
	}
	tu.PickLoss = 0
	if got, _ := flapping(PoolHealth{PickLoss: 9}, tu); got {
		t.Error("pick losses judged with pick_loss 0")
	}
}

// Quick to go agile, slow to go back: only CalmMin without flapping returns
// a pool to stable, and a flap inside that time restarts the clock.
func TestAutoClassifierHysteresis(t *testing.T) {
	now := time.Unix(10000, 0)
	c := newAutoClassifier()
	c.now = func() time.Time { return now }
	tu := xray.DefaultAutoTuning
	auto := map[string]bool{"k": true}
	flappy := []PoolHealth{{Key: "k", PickLoss: 3}}
	calm := []PoolHealth{{Key: "k", Nodes: nodesWithFlips(0, 0)}}

	if ev := c.evaluate(calm, auto, tu); len(ev) != 0 || c.agile("k") {
		t.Fatalf("calm pool switched: %+v", ev)
	}
	if ev := c.evaluate(flappy, auto, tu); len(ev) != 1 || !ev[0].agile || !c.agile("k") {
		t.Fatalf("flapping pool did not go agile: %+v", ev)
	}
	now = now.Add(time.Duration(tu.CalmMin-1) * time.Minute)
	if ev := c.evaluate(calm, auto, tu); len(ev) != 0 || !c.agile("k") {
		t.Fatalf("went back before calm time: %+v", ev)
	}
	c.evaluate(flappy, auto, tu) // a flap restarts the clock
	now = now.Add(time.Duration(tu.CalmMin-1) * time.Minute)
	if c.evaluate(calm, auto, tu); !c.agile("k") {
		t.Fatal("went back though it flapped within calm time")
	}
	now = now.Add(time.Minute)
	if ev := c.evaluate(calm, auto, tu); len(ev) != 1 || ev[0].agile || c.agile("k") {
		t.Fatalf("not back to stable after calm time: %+v", ev)
	}
	// A pool no longer on auto is forgotten.
	c.evaluate(flappy, auto, tu)
	c.evaluate(nil, map[string]bool{}, tu)
	if _, ok := c.state("k"); ok {
		t.Fatal("state kept for a pool that left auto")
	}
}

// Which pools the classifier governs, and the precedence between strategies.
func TestPoolStrategyPrecedence(t *testing.T) {
	store, sup := newTestSupervisor(t)
	for name, st := range map[string]string{"au": "", "st": xray.StrategyStable, "ag": xray.StrategyAgile} {
		if err := store.CreateSubscription(&xray.Subscription{Name: name, URL: "http://x/" + name, Enabled: true, Strategy: st}); err != nil {
			t.Fatal(err)
		}
	}
	refs := func(s string) []xray.DialerRef { r, _ := xray.ParseDialer(s); return r }
	sup.auto.pools["xraysub:au"] = &autoState{agile: true}
	for _, tc := range []struct {
		dialer, want string
		auto         bool
	}{
		{"xraysub:st", xray.StrategyStable, false},
		{"xraysub:ag", xray.StrategyAgile, false},
		{"xraysub:ag,xraysub:au", xray.StrategyAgile, false},
		{"xraysub:au", xray.StrategyAgile, true}, // the classifier's call
		{"xraysub:au,xraysub:st", xray.StrategyStable, true},
		{"xray:e1", xray.StrategyStable, true}, // entries only: auto
	} {
		r := refs(tc.dialer)
		got, auto := sup.poolStrategy(xray.DialerGroupKey(r), r)
		if got != tc.want || auto != tc.auto {
			t.Errorf("%s: %s auto=%v, want %s auto=%v", tc.dialer, got, auto, tc.want, tc.auto)
		}
	}
}

// End to end without xray: polls of a flapping auto pool switch it to agile,
// and the next resolve runs it agile.
func TestAutoSwitchesFlappingPool(t *testing.T) {
	store, sup := newTestSupervisor(t)
	sub := &xray.Subscription{Name: "s", URL: "http://x/s", Enabled: true}
	if err := store.CreateSubscription(sub); err != nil {
		t.Fatal(err)
	}
	var nodes []xray.SubNode
	for _, a := range []string{"a", "b", "c"} {
		nodes = append(nodes, xray.SubNode{Name: a, Fingerprint: a,
			Outbound: `{"protocol":"socks","settings":{"servers":[{"address":"1.1.1.1","port":1}]}}`})
	}
	if err := store.ReplaceNodes(sub.ID, nodes); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateEntry(&xray.XrayEntry{Name: "M", Enabled: true, Dialer: "xraysub:s", Outbound: `{"protocol":"freedom"}`}); err != nil {
		t.Fatal(err)
	}
	entries, _ := store.ListEntries()
	slots, err := sup.resolveDialerSlots(entries)
	if err != nil {
		t.Fatal(err)
	}
	if !slots[0].Auto || slots[0].Agile() {
		t.Fatalf("new auto pool = %+v, want auto stable", slots[0])
	}

	now := time.Now()
	sup.history.now = func() time.Time { return now }
	sup.auto.now = func() time.Time { return now }
	switched := false
	for i := 0; i < 8; i++ { // a and b alternate: two nodes flip every round
		h := map[string]bool{"a": i%2 == 0, "b": i%2 == 1, "c": true}
		sup.history.record(slots, health(h), nil)
		switched = sup.autoSwitched(slots) || switched
		now = now.Add(nodeHealthPollInterval)
	}
	if !switched || !sup.auto.agile(slots[0].Key) {
		t.Fatal("flapping auto pool not switched to agile")
	}
	slots, _ = sup.resolveDialerSlots(entries)
	if !slots[0].Auto || !slots[0].Agile() {
		t.Fatalf("after the switch pool = %+v, want auto agile", slots[0])
	}
	ph := sup.PoolHealth(0)
	if len(ph) != 1 || !ph[0].Auto || ph[0].AutoReason == "" || ph[0].AutoSince.IsZero() {
		t.Fatalf("PoolHealth = %+v, want auto reason and since", ph)
	}
}

// Against the real xray: an auto pool whose two nodes take turns going
// silent is judged flapping and switched to agile live — no restart.
func TestAutoSwitchesLiveOnRealXray(t *testing.T) {
	needRealXray(t)
	useFreeXrayPorts(t)
	store, sup := newRealSupervisor(t)
	sup.probeURL = "http://" + startHTTP204(t) + "/generate_204"
	if err := store.SetSetting(xray.SettingProbeInterval, strconv.Itoa(xray.MinProbeIntervalSec)); err != nil {
		t.Fatal(err)
	}
	tu := xray.DefaultAutoTuning
	tu.PickLoss = 0 // judge by flips alone: both nodes flip twice
	if err := store.SetAutoTuning(tu); err != nil {
		t.Fatal(err)
	}
	portA, freezeA := startFreezableNode(t)
	portB, freezeB := startFreezableNode(t)
	sub := &xray.Subscription{Name: "s", URL: "http://127.0.0.1/unused", Enabled: true}
	if err := store.CreateSubscription(sub); err != nil {
		t.Fatal(err)
	}
	var sn []xray.SubNode
	for _, p := range []int{portA, portB} {
		ob := socksMember(p)
		fp, _ := xray.Fingerprint([]byte(ob))
		sn = append(sn, xray.SubNode{Name: "n" + strconv.Itoa(p), Fingerprint: fp, Outbound: ob})
	}
	if err := store.ReplaceNodes(sub.ID, sn); err != nil {
		t.Fatal(err)
	}
	tagA, tagB := xray.SlotMemberTag(0, sn[0].Fingerprint), xray.SlotMemberTag(0, sn[1].Fingerprint)
	masterSrv, _ := startSocks5(t)
	if err := store.CreateEntry(&xray.XrayEntry{Name: "M", Enabled: true, Dialer: "xraysub:s", Outbound: socksMember(masterSrv)}); err != nil {
		t.Fatal(err)
	}
	gate := freePort(t)
	if err := store.CreateInbound(&xray.Inbound{Name: "gate", Enabled: true, Protocol: "socks", Listen: "127.0.0.1", Port: gate, Target: "master:M"}); err != nil {
		t.Fatal(err)
	}
	if err := sup.Reconcile(); err != nil {
		t.Fatal(err)
	}
	waitListening(t, gate)
	if !waitRunning(sup, 10*time.Second) {
		t.Fatal("xray did not start")
	}
	_, pid, _, _ := sup.XrayState()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		for ctx.Err() == nil {
			sup.PollNodeHealth(ctx)
			time.Sleep(time.Second)
		}
	}()
	waitObservatory(t, func(b map[string]nodeStatus) bool { return b[tagA].Alive && b[tagB].Alive })
	if sl := loadedSlot(sup); !sl.Auto || sl.Agile() {
		t.Fatalf("pool = %+v, want auto stable at start", sl)
	}
	// Each node: alive → dead → alive, one after the other.
	for _, n := range []struct {
		tag    string
		freeze func(bool)
	}{{tagA, freezeA}, {tagB, freezeB}} {
		n.freeze(true)
		waitObservatory(t, func(b map[string]nodeStatus) bool { return !b[n.tag].Alive })
		time.Sleep(3 * time.Second) // a wave lasts: let the polls see it dead
		n.freeze(false)
		waitObservatory(t, func(b map[string]nodeStatus) bool { return b[n.tag].Alive })
	}
	deadline := time.Now().Add(20 * time.Second)
	for !loadedSlot(sup).Agile() {
		if time.Now().After(deadline) {
			t.Fatalf("pool never went agile: %+v", sup.PoolHealth(0))
		}
		time.Sleep(200 * time.Millisecond)
	}
	if _, now, _, _ := sup.XrayState(); now != pid {
		t.Fatalf("xray was restarted (pid %d → %d)", pid, now)
	}
	roundTrip(t, dialVia(t, gate, startEcho(t)), "through the agile pool")
}
