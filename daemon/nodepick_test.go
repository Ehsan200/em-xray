package daemon

import (
	"context"
	"reflect"
	"strconv"
	"testing"
	"time"

	"github.com/ehsan200/em-xray/core/xray"
)

func pickSlot(keys ...string) []xray.Slot {
	sl := xray.Slot{Master: "M", Key: "k"}
	for _, k := range keys {
		sl.Members = append(sl.Members, xray.SlotMember{Key: k})
	}
	return []xray.Slot{sl}
}

// win builds a window: all pings, fails, average delay ms.
func win(all, fail int, delay int64) nodeStatus {
	st := nodeStatus{Alive: all > fail, Delay: delay}
	st.HealthPing.All, st.HealthPing.Fail = all, fail
	return st
}

func tags(m map[string]nodeStatus) map[string]nodeStatus {
	out := map[string]nodeStatus{}
	for k, v := range m {
		out[xray.SlotMemberTag(0, k)] = v
	}
	return out
}

func TestPickPrefersSteadyOverFlakyFast(t *testing.T) {
	p := newNodePicker()
	sl := pickSlot("flaky", "steady", "dead", "slow")
	for i := 0; i < 5; i++ {
		p.observe(sl, tags(map[string]nodeStatus{
			"flaky": win(3, 2, 50), "steady": win(3, 0, 300),
			"dead": win(3, 3, 0), "slow": win(3, 0, 900),
		}))
	}
	// Active pair (sorted) is the two clean nodes; the flaky fast one is only
	// the spare, and the dead one isn't picked.
	got := pickKeys(p, sl[0].Members, nil)
	if want := []string{"slow", "steady", "flaky"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("pick = %v, want %v", got, want)
	}
}

func TestPickSticksUnlessClearlyBeaten(t *testing.T) {
	p := newNodePicker()
	sl := pickSlot("a", "b", "c")
	// Spare c marginally faster than active b: b stays active.
	p.observe(sl, tags(map[string]nodeStatus{"a": win(3, 0, 100), "b": win(3, 0, 200), "c": win(3, 0, 180)}))
	if got := pickKeys(p, sl[0].Members, []string{"a", "b", "c"}); !reflect.DeepEqual(got, []string{"a", "b", "c"}) {
		t.Fatalf("spare c took active b's place over a marginal gain: %v", got)
	}
	// Spare c clearly faster than active b: they swap.
	p = newNodePicker()
	p.observe(sl, tags(map[string]nodeStatus{"a": win(3, 0, 100), "b": win(3, 0, 400), "c": win(3, 0, 100)}))
	if got := pickKeys(p, sl[0].Members, []string{"a", "b", "c"}); !reflect.DeepEqual(got, []string{"a", "c", "b"}) {
		t.Fatalf("clearly faster spare c not promoted: %v", got)
	}
}

// A newcomer must clearly beat a picked member to get into the pick at all.
func TestPickSticksPickedSet(t *testing.T) {
	p := newNodePicker()
	sl := pickSlot("a", "b", "c", "d")
	p.observe(sl, tags(map[string]nodeStatus{"a": win(3, 0, 100), "b": win(3, 0, 100), "c": win(3, 0, 300), "d": win(3, 0, 280)}))
	if got := pickKeys(p, sl[0].Members, []string{"a", "b", "c"}); !reflect.DeepEqual(got, []string{"a", "b", "c"}) {
		t.Fatalf("spare c lost to a marginally faster newcomer d: %v", got)
	}
}

func TestPickFailingIncumbentDropsOut(t *testing.T) {
	p := newNodePicker()
	sl := pickSlot("a", "b", "c", "d")
	p.observe(sl, tags(map[string]nodeStatus{"a": win(3, 1, 100), "b": win(3, 0, 300), "c": win(3, 0, 300), "d": win(3, 0, 300)}))
	got := pickKeys(p, sl[0].Members, []string{"a", "b", "c"})
	if got[0] == "a" || len(got) != 3 || contains(got, "a") {
		t.Fatalf("failing incumbent kept: %v", got)
	}
}

func contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

func TestPickIgnoresUplinkDownAndNoData(t *testing.T) {
	p := newNodePicker()
	sl := pickSlot("a", "b")
	if got := pickKeys(p, sl[0].Members, nil); got != nil {
		t.Fatalf("pick with no data = %v, want nil", got)
	}
	p.observe(sl, tags(map[string]nodeStatus{"a": win(3, 0, 100), "b": win(3, 0, 300)}))
	if p.observe(sl, tags(map[string]nodeStatus{"a": win(3, 3, 0), "b": win(3, 3, 0)})) {
		t.Fatal("all-dead poll was not ignored")
	}
	if got := pickKeys(p, sl[0].Members, nil); !reflect.DeepEqual(got, []string{"a", "b"}) {
		t.Fatalf("uplink outage changed the pick: %v", got)
	}
}

// A poll against real xray picks the live member first and applies the
// narrowed balancer live.
func TestPickAgainstRealXray(t *testing.T) {
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
	waitObservatory(t, func(b map[string]nodeStatus) bool {
		return b[xray.SlotMemberTag(0, "xray-good")].Alive && b[xray.SlotMemberTag(0, "xray-dead")].dead()
	})
	sup.PollNodeHealth(context.Background())
	if got := sup.loadedSlots[0].Picked; !contains(got, "xray-good") {
		t.Fatalf("picked = %v, want xray-good in it", got)
	}
	if restarts, _ := sup.Counters(); restarts != 0 {
		t.Fatalf("pick restarted xray %d times", restarts)
	}
}

func waitObservatory(t *testing.T, ready func(map[string]nodeStatus) bool) {
	t.Helper()
	addr := "127.0.0.1:" + strconv.Itoa(xray.MetricsPort)
	deadline := time.Now().Add(20 * time.Second)
	for {
		b, err := fetchObservatory(context.Background(), addr)
		if err == nil && ready(b) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("observatory never ready: %+v (err %v)", b, err)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// pickKeys is the pool-level stable pick's keys (no chain view).
func pickKeys(p *nodePicker, members []xray.SlotMember, incumbent []string) []string {
	keys, _ := p.pick(0, members, incumbent, nil)
	return keys
}

// poolPick is a slot's pool-level pick as incumbents.
func poolPick(sl xray.Slot) xray.MasterPick {
	return xray.MasterPick{Picked: sl.Picked, Active: sl.Active}
}
