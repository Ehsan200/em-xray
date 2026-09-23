package daemon

import (
	"net"
	"strconv"
	"testing"
	"time"

	"github.com/ehsan200/em-xray/core/xray"
	xproxy "golang.org/x/net/proxy"
)

// The point of live apply, checked against the real xray: editing, adding and
// removing entries and inbounds, adding masters and churning a pool must leave
// a stream through an untouched inbound alive, and must never restart the
// process. Everything runs on free loopback ports (useFreeXrayPorts); the
// supervisor is built over a temp store, and nothing here signals any process
// it didn't start.
func TestLiveApplyKeepsUntouchedStreams(t *testing.T) {
	needRealXray(t)
	useFreeXrayPorts(t)
	store, sup := newRealSupervisor(t)
	echo := startEcho(t)

	srv1, _ := startSocks5(t)
	srv2, _ := startSocks5(t)
	mustEntry := func(e xray.XrayEntry) *xray.XrayEntry {
		t.Helper()
		e.Enabled = true
		if err := store.CreateEntry(&e); err != nil {
			t.Fatal(err)
		}
		return &e
	}
	ports := map[string]int{}
	mustInbound := func(name, target string) {
		t.Helper()
		ports[name] = freePort(t)
		if err := store.CreateInbound(&xray.Inbound{Name: name, Enabled: true, Protocol: "socks", Listen: "127.0.0.1", Port: ports[name], Target: target}); err != nil {
			t.Fatal(err)
		}
	}
	reconcile := func(step string) {
		t.Helper()
		if err := sup.Reconcile(); err != nil {
			t.Fatalf("%s: reconcile: %v", step, err)
		}
	}

	mustEntry(xray.XrayEntry{Name: "e1", Outbound: socksMember(srv1)})
	e2 := mustEntry(xray.XrayEntry{Name: "e2", Outbound: socksMember(srv2)})
	mustInbound("keep", "xray:e1")
	mustInbound("other", "xray:e2")
	reconcile("start")
	waitListening(t, ports["keep"])
	if !waitRunning(sup, 10*time.Second) {
		t.Fatal("xray did not start")
	}
	_, pid, _, _ := sup.XrayState()
	if err := sup.waitAPIReady(10 * time.Second); err != nil {
		t.Fatal(err)
	}

	// A long-lived stream through `keep`, which none of the edits touch.
	stream := dialVia(t, ports["keep"], echo)
	roundTrip(t, stream, "before")

	var masterStream net.Conn
	check := func(step string) {
		t.Helper()
		if _, now, _, _ := sup.XrayState(); now != pid {
			t.Fatalf("%s: xray was restarted (pid %d → %d)", step, pid, now)
		}
		roundTrip(t, stream, step)
		if masterStream != nil {
			roundTrip(t, masterStream, step+" (master)")
		}
	}

	// Edit e2's outbound, add an inbound.
	srv2b, _ := startSocks5(t)
	e2.Outbound = socksMember(srv2b)
	if err := store.UpdateEntry(e2); err != nil {
		t.Fatal(err)
	}
	mustInbound("third", "xray:e1")
	reconcile("edit e2 + add inbound")
	check("edit e2 + add inbound")
	waitListening(t, ports["third"])
	roundTrip(t, dialVia(t, ports["third"], echo), "through new inbound")
	roundTrip(t, dialVia(t, ports["other"], echo), "through edited e2")

	// Add a master on a subscription pool, and an inbound to it.
	sub := &xray.Subscription{Name: "pool", URL: "http://127.0.0.1/unused", Enabled: true}
	if err := store.CreateSubscription(sub); err != nil {
		t.Fatal(err)
	}
	node1, _ := startSocks5(t)
	node2, _ := startSocks5(t)
	setNodes := func(ports ...int) {
		t.Helper()
		var nodes []xray.SubNode
		for _, p := range ports {
			ob := socksMember(p)
			fp, _ := xray.Fingerprint([]byte(ob))
			nodes = append(nodes, xray.SubNode{Name: "n" + strconv.Itoa(p), Fingerprint: fp, Outbound: ob})
		}
		if err := store.ReplaceNodes(sub.ID, nodes); err != nil {
			t.Fatal(err)
		}
	}
	setNodes(node1, node2)
	masterSrv, _ := startSocks5(t)
	mustEntry(xray.XrayEntry{Name: "m", Dialer: "xraysub:pool", Outbound: socksMember(masterSrv)})
	mustInbound("mgate", "master:m")
	reconcile("add master")
	check("add master")
	waitListening(t, ports["mgate"])
	masterStream = dialVia(t, ports["mgate"], echo)
	roundTrip(t, masterStream, "through master m")

	// A second master sorting BEFORE m, on its own pool: m keeps its slot
	// index, so its slot is untouched and its stream survives.
	mustEntry(xray.XrayEntry{Name: "a0", Dialer: "xray:e1", Outbound: socksMember(masterSrv)})
	mustInbound("agate", "master:a0")
	reconcile("add master sorting first")
	check("add master sorting first")
	waitListening(t, ports["agate"])
	roundTrip(t, dialVia(t, ports["agate"], echo), "through master a0")

	// Another master on m's pool shares its slot.
	mustEntry(xray.XrayEntry{Name: "m2", Dialer: "xraysub:pool", Outbound: socksMember(masterSrv)})
	reconcile("add alias master")
	check("add alias master")
	if n := len(sup.loadedSlots); n != 2 {
		t.Fatalf("slots = %d, want 2 (m+m2 shared, a0)", n)
	}

	// Pool churn: add a node live (the old ones stay, so m's stream stays).
	node3, _ := startSocks5(t)
	setNodes(node1, node2, node3)
	reconcile("pool grows")
	check("pool grows")
	roundTrip(t, dialVia(t, ports["mgate"], echo), "master after pool change")

	// Remove an inbound and the entry behind it.
	in, err := store.GetInboundByName("other")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.DeleteInbound(in.ID); err != nil {
		t.Fatal(err)
	}
	if err := store.DeleteEntry(e2.ID); err != nil {
		t.Fatal(err)
	}
	reconcile("remove inbound + entry")
	check("remove inbound + entry")
	if c, err := net.DialTimeout("tcp", "127.0.0.1:"+strconv.Itoa(ports["other"]), time.Second); err == nil {
		_ = c.Close()
		t.Fatal("removed inbound still listening")
	}

	restarts, live := sup.Counters()
	if restarts != 0 || live < 6 {
		t.Fatalf("counters: restarts %d, live applies %d; want 0 and >= 6", restarts, live)
	}
}

// Revoking a credential must end the sessions it opened. xray keeps accepted
// connections across an inbound replace, so this is the one inbound change
// that restarts — verified here against the real binary.
func TestRevokedCredentialCutsSessions(t *testing.T) {
	needRealXray(t)
	useFreeXrayPorts(t)
	store, sup := newRealSupervisor(t)
	echo := startEcho(t)
	srv, _ := startSocks5(t)
	if err := store.CreateEntry(&xray.XrayEntry{Name: "e", Enabled: true, Outbound: socksMember(srv)}); err != nil {
		t.Fatal(err)
	}
	port := freePort(t)
	in := xray.Inbound{Name: "g", Enabled: true, Protocol: "socks", Listen: "127.0.0.1", Port: port, Target: "xray:e", SocksUser: "alice", Password: "pw1"}
	if err := store.CreateInbound(&in); err != nil {
		t.Fatal(err)
	}
	if err := sup.Reconcile(); err != nil {
		t.Fatal(err)
	}
	waitListening(t, port)
	d, err := xproxy.SOCKS5("tcp", "127.0.0.1:"+strconv.Itoa(port), &xproxy.Auth{User: "alice", Password: "pw1"}, xproxy.Direct)
	if err != nil {
		t.Fatal(err)
	}
	c, err := d.Dial("tcp", echo)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	roundTrip(t, c, "before")

	in.Password = "pw2"
	if err := store.UpdateInbound(&in); err != nil {
		t.Fatal(err)
	}
	if err := sup.Reconcile(); err != nil {
		t.Fatal(err)
	}
	if restarts, _ := sup.Counters(); restarts != 1 {
		t.Fatalf("restarts = %d, want 1", restarts)
	}
	deadline := time.Now().Add(5 * time.Second)
	for tryRoundTrip(c, "after") == nil {
		if time.Now().After(deadline) {
			t.Fatal("session opened with the revoked password is still alive")
		}
		time.Sleep(100 * time.Millisecond)
	}
}
