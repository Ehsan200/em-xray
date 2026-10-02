package daemon

import (
	"context"
	"net"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ehsan200/em-xray/core/xray"
)

// Against the real xray: the burst observatory + leastLoad balancer must rank
// live members, leave a dead one out, spread a master's connections over the
// best two, and report a winner `emx winner` can parse.
func TestBurstBalancerSpreadsOverLiveMembers(t *testing.T) {
	needRealXray(t)
	useFreeXrayPorts(t)
	store, sup := newRealSupervisor(t)
	sup.probeURL = "http://" + startHTTP204(t) + "/generate_204"
	if err := store.SetSetting(xray.SettingProbeInterval, strconv.Itoa(xray.MinProbeIntervalSec)); err != nil {
		t.Fatal(err)
	}

	echo := startEcho(t)
	serverPort, _ := startSocks5(t) // the master's own "server"
	good1, n1 := startSocks5(t)
	good2, n2 := startSocks5(t)
	dead := freePort(t)
	for _, e := range []xray.XrayEntry{
		{Name: "good1", Enabled: true, Outbound: socksMember(good1)},
		{Name: "good2", Enabled: true, Outbound: socksMember(good2)},
		{Name: "dead", Enabled: true, Outbound: socksMember(dead)},
		{Name: "M", Enabled: true, Dialer: "xray:dead,xray:good1,xray:good2", Outbound: socksMember(serverPort)},
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

	// Wait for the observatory to rank the pool.
	var raw string
	deadline := time.Now().Add(30 * time.Second)
	for {
		ws, err := sup.Winners()
		if err == nil && len(ws) == 1 && ws[0].Node != "" {
			if ws[0].Node == "dead" {
				t.Fatalf("winner is the dead member")
			}
			raw, _ = sup.BalancerInfoRaw(xray.SlotBalTag(0, "M"))
			break
		}
		if time.Now().After(deadline) {
			raw, _ = sup.BalancerInfoRaw(xray.SlotBalTag(0, "M"))
			t.Fatalf("no winner reported; bi output:\n%s", raw)
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Logf("bi output:\n%s", raw)
	// The observatory's per-node view: two of three answer.
	deadline = time.Now().Add(20 * time.Second)
	for {
		ws, _ := sup.Winners()
		if len(ws) == 1 && ws[0].Alive == 2 && ws[0].Members == 3 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("alive count never settled at 2/3: %+v", ws)
		}
		time.Sleep(500 * time.Millisecond)
	}

	base1, base2 := n1(), n2()
	for i := 0; i < 12; i++ {
		c := dialVia(t, gate, echo)
		roundTrip(t, c, "hi "+strconv.Itoa(i))
		_ = c.Close()
	}
	d1, d2 := n1()-base1, n2()-base2
	t.Logf("connections: good1 %d, good2 %d", d1, d2)
	if d1 == 0 || d2 == 0 {
		t.Fatalf("leastLoad did not spread over the best two members (good1 %d, good2 %d)", d1, d2)
	}
}

// Before any ping has landed (here: never — the probe target swallows every
// request) leastLoad has nothing ranked, and the master must still work
// through the fallback, the pool's first member, instead of failing closed.
func TestFallbackCarriesTrafficBeforeFirstPing(t *testing.T) {
	needRealXray(t)
	useFreeXrayPorts(t)
	store, sup := newRealSupervisor(t)
	sup.probeURL = "http://" + startSilent(t) + "/generate_204"

	echo := startEcho(t)
	serverPort, _ := startSocks5(t)
	node, served := startSocks5(t)
	for _, e := range []xray.XrayEntry{
		{Name: "node", Enabled: true, Outbound: socksMember(node)},
		{Name: "M", Enabled: true, Dialer: "xray:node", Outbound: socksMember(serverPort)},
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
	if ws, _ := sup.Winners(); len(ws) != 1 || ws[0].Node != "" {
		t.Fatalf("expected nothing ranked yet, got %+v", ws)
	}
	before := served()
	roundTrip(t, dialVia(t, gate, echo), "via fallback")
	if served() == before {
		t.Fatal("master traffic did not go through the pool member")
	}
}

// startSilent accepts TCP connections and never answers.
func startSilent(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var held []net.Conn
	t.Cleanup(func() {
		_ = ln.Close()
		mu.Lock()
		defer mu.Unlock()
		for _, c := range held {
			_ = c.Close()
		}
	})
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			held = append(held, c)
			mu.Unlock()
		}
	}()
	return ln.Addr().String()
}

// Against the real xray, once the daemon has ranked the pool: traffic rides
// only the active pair, never the spare; an active member that dies is dropped
// by xray itself on the next ping round (no daemon poll in between); and with
// both active members dead the spare carries the traffic as fallback.
func TestPickedBalancerFailsOverToSpare(t *testing.T) {
	needRealXray(t)
	useFreeXrayPorts(t)
	store, sup := newRealSupervisor(t)
	sup.probeURL = "http://" + startHTTP204(t) + "/generate_204"
	if err := store.SetSetting(xray.SettingProbeInterval, strconv.Itoa(xray.MinProbeIntervalSec)); err != nil {
		t.Fatal(err)
	}

	echo := startEcho(t)
	serverPort, _ := startSocks5(t)
	type node struct {
		port   int
		served func() int64
		kill   func()
	}
	nodes := map[string]node{}
	for _, name := range []string{"n1", "n2", "n3"} {
		port, served, kill := startKillableSocks5(t)
		nodes["xray-"+name] = node{port, served, kill}
		e := xray.XrayEntry{Name: name, Enabled: true, Outbound: socksMember(port)}
		if err := store.CreateEntry(&e); err != nil {
			t.Fatal(err)
		}
	}
	m := xray.XrayEntry{Name: "M", Enabled: true, Dialer: "xray:n1,xray:n2,xray:n3", Outbound: socksMember(serverPort)}
	if err := store.CreateEntry(&m); err != nil {
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
	waitObservatory(t, func(b map[string]nodeStatus) bool {
		for k := range nodes {
			if !b[xray.SlotMemberTag(0, k)].Alive {
				return false
			}
		}
		return true
	})
	sup.PollNodeHealth(context.Background())
	picked := sup.loadedSlots[0].Picked
	if len(picked) != 3 {
		t.Fatalf("picked = %v, want 2 active + 1 spare", picked)
	}
	a1, a2, spare := nodes[picked[0]], nodes[picked[1]], nodes[picked[2]]
	t.Logf("active %s %s, spare %s", picked[0], picked[1], picked[2])

	send := func(n int) {
		t.Helper()
		for i := 0; i < n; i++ {
			c := dialVia(t, gate, echo)
			roundTrip(t, c, "hi "+strconv.Itoa(i))
			_ = c.Close()
		}
	}
	snap := func() [3]int64 { return [3]int64{a1.served(), a2.served(), spare.served()} }
	// The probe's own pings are counted too (at most one per member per
	// interval), hence the slack on the spare below.
	diff := func(b [3]int64) [3]int64 { n := snap(); return [3]int64{n[0] - b[0], n[1] - b[1], n[2] - b[2]} }

	b := snap()
	send(16)
	d := diff(b)
	t.Logf("healthy: %v", d)
	if d[0] == 0 || d[1] == 0 {
		t.Fatalf("traffic not spread over the active pair: %v", d)
	}
	if d[2] > 2 { // at most the spare's own pings
		t.Fatalf("spare carried traffic while the active pair was healthy: %v", d)
	}

	waitDead := func(key string) {
		t.Helper()
		waitObservatory(t, func(st map[string]nodeStatus) bool { return !st[xray.SlotMemberTag(0, key)].Alive })
	}
	a1.kill()
	waitDead(picked[0])
	b = snap()
	send(8)
	d = diff(b)
	t.Logf("one active dead: %v", d)
	if d[1] < 8 {
		t.Fatalf("surviving active member did not take over: %v", d)
	}

	a2.kill()
	waitDead(picked[1])
	b = snap()
	send(4)
	d = diff(b)
	t.Logf("both active dead: %v", d)
	if d[2] < 4 {
		t.Fatalf("spare did not carry traffic as fallback: %v", d)
	}
	if restarts, _ := sup.Counters(); restarts != 0 {
		t.Fatalf("failover restarted xray %d times", restarts)
	}
}

// startKillableSocks5 is startSocks5 whose kill closes the listener and every
// connection it accepted, as if the node went away.
func startKillableSocks5(t *testing.T) (port int, served func() int64, kill func()) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var conns []net.Conn
	var n atomic.Int64
	kill = func() {
		_ = ln.Close()
		mu.Lock()
		defer mu.Unlock()
		for _, c := range conns {
			_ = c.Close()
		}
	}
	t.Cleanup(kill)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			n.Add(1)
			mu.Lock()
			conns = append(conns, c)
			mu.Unlock()
			go serveSocks5(c)
		}
	}()
	return ln.Addr().(*net.TCPAddr).Port, n.Load, kill
}
