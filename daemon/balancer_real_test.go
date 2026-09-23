package daemon

import (
	"net"
	"strconv"
	"sync"
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
			raw, _ = sup.BalancerInfoRaw(xray.SlotBalTag(0))
			break
		}
		if time.Now().After(deadline) {
			raw, _ = sup.BalancerInfoRaw(xray.SlotBalTag(0))
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
