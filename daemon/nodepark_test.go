package daemon

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/ehsan200/em-xray/core/xray"
)

func parkSlot(keys ...string) []xray.Slot {
	ms := make([]xray.SlotMember, len(keys))
	for i, k := range keys {
		ms[i] = xray.SlotMember{Key: k}
	}
	return []xray.Slot{{Master: "m", Index: 0, Members: ms}}
}

func health(alive map[string]bool) map[string]nodeStatus {
	out := map[string]nodeStatus{}
	for k, a := range alive {
		var st nodeStatus
		st.Alive = a
		st.HealthPing.All = 3
		if !a {
			st.HealthPing.Fail = 3
		}
		out[xray.SlotMemberTag(0, k)] = st
	}
	return out
}

func TestNodeParkerLifecycle(t *testing.T) {
	now := time.Unix(0, 0)
	p := newNodeParker()
	p.now = func() time.Time { return now }
	slots := parkSlot("good", "dead")
	h := health(map[string]bool{"good": true, "dead": false})

	if ev := p.observe(slots, h); len(ev) != 0 {
		t.Fatalf("parked on first sight: %+v", ev)
	}
	now = now.Add(nodeDeadBeforePark)
	ev := p.observe(slots, h)
	if len(ev) != 1 || ev[0].key != "dead" || !ev[0].parked || ev[0].for_ != nodeParkInitial {
		t.Fatalf("events = %+v, want dead parked for %s", ev, nodeParkInitial)
	}
	if _, ok := p.parked()["dead"]; !ok {
		t.Fatalf("dead not reported parked")
	}

	// Time up → back on trial.
	now = now.Add(nodeParkInitial)
	if ev := p.release(); len(ev) != 1 || ev[0].parked {
		t.Fatalf("release = %+v, want one trial", ev)
	}
	if _, ok := p.parked()["dead"]; ok {
		t.Fatalf("still parked after release")
	}
	// Fails through the (short) trial → parked again, twice as long.
	p.observe(slots, h)
	now = now.Add(nodeTrialWindow)
	ev = p.observe(slots, h)
	if len(ev) != 1 || ev[0].for_ != 2*nodeParkInitial {
		t.Fatalf("re-park = %+v, want %s", ev, 2*nodeParkInitial)
	}

	// Recovers on a later trial → one live ping clears the history.
	now = now.Add(2 * nodeParkInitial)
	p.release()
	p.observe(slots, health(map[string]bool{"good": true, "dead": true}))
	if len(p.nodes) != 0 {
		t.Fatalf("recovered node kept history: %+v", p.nodes)
	}
}

// Everything dead at once is an uplink outage: nothing may be parked, or the
// pool would stay empty after the link comes back.
func TestNodeParkerNeverParksWithoutLiveSibling(t *testing.T) {
	now := time.Unix(0, 0)
	p := newNodeParker()
	p.now = func() time.Time { return now }
	slots := parkSlot("a", "b")
	h := health(map[string]bool{"a": false, "b": false})
	p.observe(slots, h)
	now = now.Add(time.Hour)
	if ev := p.observe(slots, h); len(ev) != 0 {
		t.Fatalf("parked during total outage: %+v", ev)
	}
}

func TestNodeParkerCapsBackoff(t *testing.T) {
	now := time.Unix(0, 0)
	p := newNodeParker()
	p.now = func() time.Time { return now }
	slots := parkSlot("good", "dead")
	h := health(map[string]bool{"good": true, "dead": false})
	p.observe(slots, h)
	now = now.Add(nodeDeadBeforePark)
	p.observe(slots, h)
	var last time.Duration
	for i := 0; i < 10; i++ {
		now = now.Add(nodeParkMax)
		p.release()
		p.observe(slots, h)
		now = now.Add(nodeTrialWindow)
		if ev := p.observe(slots, h); len(ev) == 1 {
			last = ev[0].for_
		}
	}
	if last != nodeParkMax {
		t.Fatalf("park length = %s, want capped at %s", last, nodeParkMax)
	}
}

func TestWithoutParkedKeepsLastMember(t *testing.T) {
	ms := []xray.SlotMember{{Key: "a"}, {Key: "b"}}
	until := time.Now().Add(time.Hour)
	if got := withoutParked(ms, map[string]time.Time{"a": until}); len(got) != 1 || got[0].Key != "b" {
		t.Fatalf("got %+v", got)
	}
	if got := withoutParked(ms, map[string]time.Time{"a": until, "b": until}); len(got) != 2 {
		t.Fatalf("emptied the pool: %+v", got)
	}
}

func TestParseObservatoryVars(t *testing.T) {
	body := []byte(`{"cmdline":[],"observatory":{"slot0-out-dead":{"outbound_tag":"slot0-out-dead","health_ping":{"all":3,"fail":3}},"slot0-out-good":{"alive":true,"delay":517,"health_ping":{"all":3}}}}`)
	m, err := parseObservatoryVars(body)
	if err != nil {
		t.Fatal(err)
	}
	if !m["slot0-out-dead"].dead() || m["slot0-out-good"].dead() || !m["slot0-out-good"].Alive {
		t.Fatalf("parsed = %+v", m)
	}
}

// Against the real xray: the generated metrics endpoint exposes per-node
// health in the shape the parker reads (a live and a dead member, pinged at a
// local 204 target), and parking the dead node removes it live — no restart.
func TestParkDeadNodeAgainstRealXray(t *testing.T) {
	needRealXray(t)
	useFreeXrayPorts(t)
	store, sup := newRealSupervisor(t)
	sup.probeURL = "http://" + startHTTP204(t) + "/generate_204"
	if err := store.SetSetting(xray.SettingProbeInterval, strconv.Itoa(xray.MinProbeIntervalSec)); err != nil {
		t.Fatal(err)
	}
	good, _ := startSocks5(t)
	srv, _ := startSocks5(t)
	for _, e := range []xray.XrayEntry{
		{Name: "good", Enabled: true, Outbound: socksMember(good)},
		{Name: "dead", Enabled: true, Outbound: socksMember(freePort(t))},
		{Name: "M", Enabled: true, Dialer: "xray:good,xray:dead", Outbound: socksMember(srv)},
	} {
		e := e
		if err := store.CreateEntry(&e); err != nil {
			t.Fatal(err)
		}
	}
	gate := freePort(t)
	if err := store.CreateInbound(&xray.Inbound{Name: "gate", Enabled: true, Protocol: "socks", Listen: "127.0.0.1", Port: gate, Target: "master:M"}); err != nil {
		t.Fatal(err)
	}
	if err := sup.Reconcile(); err != nil {
		t.Fatal(err)
	}
	waitListening(t, gate)

	addr := "127.0.0.1:" + strconv.Itoa(xray.MetricsPort)
	var byTag map[string]nodeStatus
	deadline := time.Now().Add(20 * time.Second)
	for {
		var err error
		byTag, err = fetchObservatory(context.Background(), addr)
		if err == nil && byTag[xray.SlotMemberTag(0, "xray-good")].Alive && byTag[xray.SlotMemberTag(0, "xray-dead")].dead() {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("observatory never reported good alive + dead dead: %+v (err %v)", byTag, err)
		}
		time.Sleep(500 * time.Millisecond)
	}

	// Fast-forward the parker past the dead window and poll.
	now := time.Now()
	sup.parker.now = func() time.Time { return now }
	sup.PollNodeHealth(context.Background()) // first dead sighting
	now = now.Add(nodeDeadBeforePark)
	sup.PollNodeHealth(context.Background())
	if _, ok := sup.ParkedNodes()["xray-dead"]; !ok {
		t.Fatalf("dead node not parked; parked = %v", sup.ParkedNodes())
	}
	if n := len(sup.loadedSlots[0].Members); n != 1 {
		t.Fatalf("pool = %d members after parking, want 1", n)
	}
	if restarts, live := sup.Counters(); restarts != 0 || live != 1 {
		t.Fatalf("parking: restarts %d, live applies %d; want 0 and 1", restarts, live)
	}
}

// A node a refresh dropped is forgotten; a parked one (absent by design) is not.
func TestNodeParkerForgetsDepartedNodes(t *testing.T) {
	now := time.Unix(0, 0)
	p := newNodeParker()
	p.now = func() time.Time { return now }
	h := health(map[string]bool{"good": true, "dead": false, "gone": false})
	p.observe(parkSlot("good", "dead", "gone"), h)
	now = now.Add(nodeDeadBeforePark)
	p.observe(parkSlot("good", "dead", "gone"), h) // parks dead + gone
	p.observe(parkSlot("good"), h)                 // both absent: parked, kept
	if len(p.parked()) != 2 {
		t.Fatalf("parked nodes forgotten: %v", p.parked())
	}
	now = now.Add(nodeParkInitial)
	p.release()
	p.observe(parkSlot("good", "dead"), h) // "gone" left the pool while on trial
	if _, ok := p.nodes["gone"]; ok {
		t.Fatal("departed node kept")
	}
	if _, ok := p.nodes["dead"]; !ok {
		t.Fatal("node on trial forgotten")
	}
}
