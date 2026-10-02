package daemon

import (
	"context"
	"net"
	"reflect"
	"sort"
	"strconv"
	"testing"
	"time"

	"github.com/ehsan200/em-xray/core/xray"
)

// A member that fails the master's chain drops below every member that might
// carry it, and out of the active set, however fast its pings.
func TestPickDemotesChainBadMember(t *testing.T) {
	p := newNodePicker()
	sl := pickSlot("fast", "mid", "slow")
	for range 3 {
		p.observe(sl, tags(map[string]nodeStatus{
			"fast": win(3, 0, 50), "mid": win(3, 0, 200), "slow": win(3, 0, 400),
		}))
	}
	if got := pickKeys(p, sl[0].Members, nil); !reflect.DeepEqual(got, []string{"fast", "mid", "slow"}) {
		t.Fatalf("no chain view: %v", got)
	}
	cv := chainView{"fast": {class: chainBad}}
	got, active := p.pick(0, sl[0].Members, nil, cv)
	// …and isn't even the spare (the balancer's fallback).
	if !reflect.DeepEqual(got, []string{"mid", "slow"}) || active != 2 {
		t.Fatalf("fast fails the chain: pick %v active %d", got, active)
	}
	// Proven carriers lead the unproven, by chain latency.
	cv = chainView{"fast": {class: chainBad}, "slow": {class: chainGood, cost: 900}}
	got, active = p.pick(0, sl[0].Members, nil, cv)
	if !reflect.DeepEqual(got, []string{"mid", "slow"}) || active != 2 {
		t.Fatalf("pick %v active %d", got, active)
	}
	// (the active pair is listed by key; the ranking itself shows with a
	// third healthy member that the proven one beats the faster unproven)
	sl4 := pickSlot("fast", "mid", "slow", "x")
	for range 3 {
		p.observe(sl4, tags(map[string]nodeStatus{
			"fast": win(3, 0, 50), "mid": win(3, 0, 200), "slow": win(3, 0, 400), "x": win(3, 0, 100),
		}))
	}
	got, _ = p.pick(0, sl4[0].Members, nil, chainView{"fast": {class: chainBad}, "slow": {class: chainGood, cost: 900}})
	if !reflect.DeepEqual(got[:2], []string{"slow", "x"}) {
		t.Fatalf("proven carrier must lead the unproven: %v", got)
	}
	// Only one member carries the master: it alone is active, the bad ones
	// stay as spare/fallback.
	cv = chainView{"fast": {class: chainBad}, "mid": {class: chainBad}, "slow": {class: chainGood, cost: 900}}
	got, active = p.pick(0, sl[0].Members, nil, cv)
	if !reflect.DeepEqual(got, []string{"slow"}) || active != 1 {
		t.Fatalf("one carrier: pick %v active %d", got, active)
	}
	// Every member fails: fail open over the pool's own ranking.
	cv = chainView{"fast": {class: chainBad}, "mid": {class: chainBad}, "slow": {class: chainBad}}
	got, active = p.pick(0, sl[0].Members, nil, cv)
	if active != 2 || len(got) != 3 {
		t.Fatalf("all bad: pick %v active %d", got, active)
	}
}

func TestPickAgileSkipsChainBad(t *testing.T) {
	p := newNodePicker()
	sl := agileSlot("a", "b", "c")
	p.observe(sl, tags(map[string]nodeStatus{"a": win(2, 0, 50), "b": win(2, 0, 100), "c": win(2, 2, 0)}))
	got, active := p.pickFor(sl[0], xray.MasterPick{}, chainView{"a": {class: chainBad}})
	if !reflect.DeepEqual(got, []string{"b", "c"}) || active != 1 {
		t.Fatalf("agile with a chain-bad member: pick %v active %d", got, active)
	}
	// Nothing alive carries it: the live chain-bad member beats the dead.
	got, active = p.pickFor(sl[0], xray.MasterPick{}, chainView{"a": {class: chainBad}, "b": {class: chainBad}})
	if active != 2 || got[0] == "c" || got[1] == "c" {
		t.Fatalf("all live members bad: pick %v active %d", got, active)
	}
}

func TestChainScores(t *testing.T) {
	c := newChainScores()
	now := time.Now()
	members := []xray.SlotMember{{Key: "n1"}, {Key: "n2"}}
	if v := c.view("M", members); v != nil {
		t.Fatalf("fresh store has a view: %v", v)
	}
	c.record("M", "n1", 2, 2, 300, nil, now)
	c.record("M", "n2", 0, 2, 0, context.DeadlineExceeded, now)
	v := c.view("M", members)
	if v.of("n1").class != chainGood || v.of("n2").class != chainBad {
		t.Fatalf("view = %v", v)
	}
	if c.due("M", "n1", now.Add(time.Minute)) || !c.due("M", "n1", now.Add(chainProbeEvery)) || !c.due("N", "n1", now) {
		t.Error("due")
	}
	// One failed round doesn't condemn a carrier; two in a row do.
	c.record("M", "n1", 0, 2, 0, nil, now)
	if c.view("M", members).of("n1").class != chainGood {
		t.Error("one failed round flipped n1")
	}
	c.record("M", "n1", 0, 2, 0, nil, now)
	if c.view("M", members).of("n1").class != chainBad {
		t.Error("two failed rounds kept n1")
	}
	c.renameMaster("M", "M2")
	c.renameMember("n2", "n3")
	if st := c.status("M2", "n3"); !st.Probed || st.Good {
		t.Errorf("renamed status = %+v", st)
	}
	c.keep([]xray.Slot{{Master: "M2", Members: []xray.SlotMember{{Key: "n1"}}}})
	if c.status("M2", "n3").Probed || !c.status("M2", "n1").Probed {
		t.Error("keep pruned the wrong scores")
	}
}

// Two masters share one pool of three nodes, all answering the observatory:
// n1 reaches only M1's server, n2 only M2's, and n3 reaches both but its
// connections are choked after 4 KB (filtering that lets a ping through and
// kills real transfers). After a chain probe round each master rides the one
// node that carries it, n3 is no master's, and every connection through either
// master gets through.
func TestChainProbeSteersEachMaster(t *testing.T) {
	needRealXray(t)
	useFreeXrayPorts(t)
	store, sup := newRealSupervisor(t)
	web := startHTTPBytes(t, 64<<10)
	sup.probeURL = "http://" + web + "/generate_204"
	sup.chainURL = "http://" + web + "/bytes"
	if err := store.SetSetting(xray.SettingProbeInterval, strconv.Itoa(xray.MinProbeIntervalSec)); err != nil {
		t.Fatal(err)
	}
	_, webPortS, _ := net.SplitHostPort(web)
	webPort, _ := strconv.Atoi(webPortS)

	echo := startEcho(t)
	s1, _ := startSocks5(t)
	s2, _ := startSocks5(t)
	node := func(server int, choke int64) int {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = ln.Close() })
		go func() {
			for {
				c, err := ln.Accept()
				if err != nil {
					return
				}
				go serveSocks5Only(c, func(p int) bool { return server == 0 || p == server || p == webPort }, choke)
			}
		}()
		return ln.Addr().(*net.TCPAddr).Port
	}
	for _, e := range []xray.XrayEntry{
		{Name: "n1", Enabled: true, Outbound: socksMember(node(s1, 0))},
		{Name: "n2", Enabled: true, Outbound: socksMember(node(s2, 0))},
		{Name: "n3", Enabled: true, Outbound: socksMember(node(0, 4<<10))},
		{Name: "M1", Enabled: true, Dialer: "xray:n1,xray:n2,xray:n3", Outbound: socksMember(s1)},
		{Name: "M2", Enabled: true, Dialer: "xray:n1,xray:n2,xray:n3", Outbound: socksMember(s2)},
		// M3's own server is down: every node fails it, none is to blame.
		{Name: "M3", Enabled: true, Dialer: "xray:n1,xray:n2,xray:n3", Outbound: socksMember(freePort(t))},
	} {
		if err := store.CreateEntry(&e); err != nil {
			t.Fatal(err)
		}
	}
	gates := map[string]int{"M1": freePort(t), "M2": freePort(t)}
	for m, port := range gates {
		if err := store.CreateInbound(&xray.Inbound{Name: "gate-" + m, Enabled: true, Protocol: "socks", Listen: "127.0.0.1", Port: port, Target: "master:" + m}); err != nil {
			t.Fatal(err)
		}
	}
	if err := sup.Reconcile(); err != nil {
		t.Fatal(err)
	}
	for _, port := range gates {
		waitListening(t, port)
	}
	waitObservatory(t, func(b map[string]nodeStatus) bool {
		return b[xray.SlotMemberTag(0, "xray-n1")].Alive && b[xray.SlotMemberTag(0, "xray-n2")].Alive &&
			b[xray.SlotMemberTag(0, "xray-n3")].Alive
	})
	sup.PollNodeHealth(context.Background())
	if sl := loadedSlot(sup); sl.ActiveCount() != 2 || len(sl.MasterPicks) != 0 {
		t.Fatalf("before chain probing both nodes are active for both: %+v", sl)
	}

	sup.ProbeChains(context.Background())
	sl := loadedSlot(sup)
	for m, want := range map[string]string{"M1": "xray-n1", "M2": "xray-n2"} {
		p := sl.PickOf(m)
		if p.ActiveCount() != 1 || p.Picked[0] != want || len(p.Picked) != 1 {
			t.Fatalf("%s pick = %+v, want %s alone (no failed spare)", m, p, want)
		}
		st := sup.ChainStatuses()[m]
		if !st[want].Good || len(st) != 3 || st["xray-n3"].Good {
			t.Errorf("%s chain statuses = %+v", m, st)
		}
		t.Logf("%s via choked n3: %s", m, st["xray-n3"].Err)
	}
	if st := sup.ChainStatuses()["M3"]; len(st) != 0 {
		t.Errorf("M3's server is down, yet nodes were judged: %+v", st)
	}
	if _, own := sl.MasterPicks["M3"]; own {
		t.Errorf("M3 must follow the pool's pick, got its own")
	}
	ws, err := sup.Winners()
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range ws {
		if w.Master == "M3" {
			continue
		}
		if want := map[string]string{"M1": "n1", "M2": "n2"}[w.Master]; w.Node != want {
			t.Errorf("winner of %s = %q, want %q", w.Master, w.Node, want)
		}
	}
	for m, port := range gates {
		for i := range 12 {
			c := dialVia(t, port, echo)
			if err := tryRoundTrip(c, m+" "+strconv.Itoa(i)); err != nil {
				t.Fatalf("%s connection %d failed: %v", m, i, err)
			}
			_ = c.Close()
		}
	}
}

// Per master: its pick, then the pool's order until chainWindow members not
// known to fail it are covered; plus the best proven member as control.
func TestDuePairsWindow(t *testing.T) {
	s := &Supervisor{picker: newNodePicker(), chain: newChainScores()}
	keys := []string{"a", "b", "c", "d", "e", "f", "g", "h"}
	sl := pickSlot(keys...)
	st := map[string]nodeStatus{}
	for i, k := range keys {
		st[k] = win(2, 0, int64(100*(i+1))) // a fastest … h slowest
	}
	st["h"] = win(2, 2, 0) // dead
	s.picker.observe(sl, tags(st))
	now := time.Now()
	pairKeys := func() []string {
		var out []string
		for _, p := range s.duePairs(sl, now) {
			k := p.member.Key
			if p.control {
				k += "*"
			}
			out = append(out, k)
		}
		sort.Strings(out)
		return out
	}
	if got := pairKeys(); !reflect.DeepEqual(got, []string{"a", "b", "c", "d"}) {
		t.Fatalf("fresh master: %v", got)
	}
	// a and b fail it, c carries it, all just probed: the window reaches
	// past the failures to e and f, with c as the control.
	s.chain.record("M", "a", 0, 1, 0, nil, now)
	s.chain.record("M", "b", 0, 1, 0, nil, now)
	s.chain.record("M", "c", 1, 1, 300, nil, now)
	if got := pairKeys(); !reflect.DeepEqual(got, []string{"c*", "d", "e", "f"}) {
		t.Fatalf("after a round: %v", got)
	}
	// Nothing due: no round, so no control either.
	for _, k := range []string{"d", "e", "f"} {
		s.chain.record("M", k, 1, 1, 400, nil, now)
	}
	if got := pairKeys(); len(got) != 0 {
		t.Fatalf("nothing due: %v", got)
	}
}
