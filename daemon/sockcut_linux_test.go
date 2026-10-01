//go:build linux

package daemon

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/ehsan200/em-xray/core/xray"
	"golang.org/x/sys/unix"
)

// skipUnlessCanCut skips when this process may not close sockets (not root,
// no ptrace rights) or the kernel lacks the mechanism.
func skipUnlessCanCut(t *testing.T, err error) {
	t.Helper()
	if errors.Is(err, unix.EPERM) || errors.Is(err, unix.EACCES) || errors.Is(err, unix.EOPNOTSUPP) || errors.Is(err, unix.ENOSYS) {
		t.Skipf("cannot destroy sockets here: %v", err)
	}
}

// dialFrom opens a TCP connection to the echo server from a given loopback
// source address.
func dialFrom(t *testing.T, src, echo string) net.Conn {
	t.Helper()
	d := net.Dialer{LocalAddr: &net.TCPAddr{IP: net.ParseIP(src)}, Timeout: 5 * time.Second}
	c, err := d.Dial("tcp", echo)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// waitClosed fails unless c is closed within timeout.
func waitClosed(t *testing.T, c net.Conn, what string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for tryRoundTrip(c, "ping") == nil {
		if time.Now().After(deadline) {
			t.Fatalf("%s: still open after %v", what, timeout)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// Both ways of closing sockets, on this test process's own connections: only
// the ones from or to the given address close.
func TestCutLoopbackSockets(t *testing.T) {
	for _, tc := range []struct {
		name string
		cut  func(match sockMatch) (int, error)
	}{
		{"sock_destroy", destroySockets},
		{"pidfd_getfd", func(match sockMatch) (int, error) { return shutdownProcSockets(os.Getpid(), match) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			echo := startEcho(t)
			cut := dialFrom(t, "127.77.1.2", echo)
			keep := dialFrom(t, "127.0.0.1", echo)
			other := dialFrom(t, "127.77.1.3", echo)
			roundTrip(t, cut, "before")

			addr := netip.AddrFrom4([4]byte{127, 77, 1, 2})
			n, err := tc.cut(func(src, dst netip.AddrPort) bool { return src.Addr() == addr || dst.Addr() == addr })
			skipUnlessCanCut(t, err)
			if err != nil {
				t.Fatal(err)
			}
			// The client socket, and the echo server's end unless the first
			// one's reset already closed it.
			if n < 1 {
				t.Errorf("closed %d sockets", n)
			}
			waitClosed(t, cut, "cut connection", 2*time.Second)
			roundTrip(t, keep, "untouched 127.0.0.1")
			roundTrip(t, other, "untouched other address")
		})
	}
}

// Cutting by remote endpoint closes only connections to that address AND
// port: a second server on the same address stays up.
func TestCutRemoteEndpoints(t *testing.T) {
	echoA, echoB := startEcho(t), startEcho(t)
	toA := dialFrom(t, "127.0.0.1", echoA)
	toB := dialFrom(t, "127.0.0.1", echoB)
	roundTrip(t, toA, "before")
	ep := netip.MustParseAddrPort(echoA)
	n, err := cutRemoteEndpoints(os.Getpid(), []netip.AddrPort{ep})
	skipUnlessCanCut(t, err)
	if err != nil {
		t.Fatal(err)
	}
	if n < 1 {
		t.Errorf("closed %d sockets", n)
	}
	waitClosed(t, toA, "connection to the dead endpoint", 2*time.Second)
	roundTrip(t, toB, "same address, other port")
}

// The whole feature against the real xray: two masters share a pool, each
// with a long-lived stream open. Moving one master to another pool is applied
// live (no restart), closes that master's stream at once so its client
// reconnects, the reconnect rides the new pool, and the other master's stream
// is untouched.
func TestDialerChangeCutsThatMastersConnections(t *testing.T) {
	needRealXray(t)
	if os.Geteuid() != 0 {
		t.Skip("closing xray's sockets needs root")
	}
	useFreeXrayPorts(t)
	store, sup := newRealSupervisor(t)
	echo := startEcho(t)

	mkSub := func(name string, nodes ...int) {
		t.Helper()
		sub := &xray.Subscription{Name: name, URL: "http://127.0.0.1/unused", Enabled: true}
		if err := store.CreateSubscription(sub); err != nil {
			t.Fatal(err)
		}
		var sn []xray.SubNode
		for _, p := range nodes {
			ob := socksMember(p)
			fp, _ := xray.Fingerprint([]byte(ob))
			sn = append(sn, xray.SubNode{Name: "n" + strconv.Itoa(p), Fingerprint: fp, Outbound: ob})
		}
		if err := store.ReplaceNodes(sub.ID, sn); err != nil {
			t.Fatal(err)
		}
	}
	oldNode, _ := startSocks5(t)
	newNode, newAccepted := startSocks5(t)
	mkSub("old", oldNode)
	mkSub("new", newNode)

	masterSrv, _ := startSocks5(t)
	ports := map[string]int{}
	entries := map[string]*xray.XrayEntry{}
	for _, m := range []string{"m1", "m2"} {
		e := &xray.XrayEntry{Name: m, Enabled: true, Dialer: "xraysub:old", Outbound: socksMember(masterSrv)}
		if err := store.CreateEntry(e); err != nil {
			t.Fatal(err)
		}
		entries[m] = e
		ports[m] = freePort(t)
		if err := store.CreateInbound(&xray.Inbound{Name: "g" + m, Enabled: true, Protocol: "socks", Listen: "127.0.0.1", Port: ports[m], Target: "master:" + m}); err != nil {
			t.Fatal(err)
		}
	}
	if err := sup.Reconcile(); err != nil {
		t.Fatal(err)
	}
	for _, m := range []string{"m1", "m2"} {
		waitListening(t, ports[m])
	}
	if !waitRunning(sup, 10*time.Second) {
		t.Fatal("xray did not start")
	}
	_, pid, _, _ := sup.XrayState()
	if err := sup.waitAPIReady(10 * time.Second); err != nil {
		t.Fatal(err)
	}
	s1 := dialVia(t, ports["m1"], echo)
	s2 := dialVia(t, ports["m2"], echo)
	roundTrip(t, s1, "m1 before")
	roundTrip(t, s2, "m2 before")

	entries["m1"].Dialer = "xraysub:new"
	if err := store.UpdateEntry(entries["m1"]); err != nil {
		t.Fatal(err)
	}
	before := newAccepted()
	if err := sup.Reconcile(); err != nil {
		t.Fatal(err)
	}
	if _, now, _, _ := sup.XrayState(); now != pid {
		t.Fatalf("xray was restarted (pid %d → %d)", pid, now)
	}
	if restarts, _ := sup.Counters(); restarts != 0 {
		t.Fatalf("restarts = %d, want 0", restarts)
	}

	waitClosed(t, s1, "m1's stream after its dialer changed", 3*time.Second)
	roundTrip(t, s2, "m2 after m1's dialer changed")

	// The client reconnects: through the new pool.
	roundTrip(t, dialVia(t, ports["m1"], echo), "m1 reconnect")
	if newAccepted() <= before {
		t.Fatal("m1's reconnect did not go through the new pool")
	}
	roundTrip(t, s2, "m2 at the end")
}

// An agile pool against the real xray: a stream rides node A; when A goes
// silent the daemon sees it die, closes the stream so the client reconnects,
// and the reconnect rides node B — no restart, no waiting for the idle timeout.
func TestAgileCutsDeadMemberConnections(t *testing.T) {
	needRealXray(t)
	if os.Geteuid() != 0 {
		t.Skip("closing xray's sockets needs root")
	}
	useFreeXrayPorts(t)
	store, sup := newRealSupervisor(t)
	sup.probeURL = "http://" + startHTTP204(t) + "/generate_204"
	if err := store.SetSetting(xray.SettingProbeInterval, strconv.Itoa(xray.MinProbeIntervalSec)); err != nil {
		t.Fatal(err)
	}
	echo := startEcho(t)
	portA, freezeA := startFreezableNode(t)
	portB, freezeB := startFreezableNode(t)

	sub := &xray.Subscription{Name: "wild", URL: "http://127.0.0.1/unused", Enabled: true, Strategy: xray.StrategyAgile}
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
	if err := store.CreateEntry(&xray.XrayEntry{Name: "M", Enabled: true, Dialer: "xraysub:wild", Outbound: socksMember(masterSrv)}); err != nil {
		t.Fatal(err)
	}
	gate := freePort(t)
	if err := store.CreateInbound(&xray.Inbound{Name: "gate", Enabled: true, Protocol: "socks", Listen: "127.0.0.1", Port: gate, Target: "master:M"}); err != nil {
		t.Fatal(err)
	}

	freezeB(true) // the stream must start on A
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
	waitObservatory(t, func(b map[string]nodeStatus) bool { return b[tagA].Alive && !b[tagB].Alive })
	if sl := loadedSlot(sup); !sl.Agile() {
		t.Fatalf("pool strategy = %q, want agile", sl.Strategy)
	}

	s := dialVia(t, gate, echo)
	roundTrip(t, s, "on A")

	freezeB(false)
	deadline := time.Now().Add(40 * time.Second)
	for loadedSlot(sup).ActiveCount() != 2 {
		if time.Now().After(deadline) {
			t.Fatalf("B never became active: %+v", loadedSlot(sup))
		}
		time.Sleep(200 * time.Millisecond)
	}
	roundTrip(t, s, "still on A")

	freezeA(true)
	frozeAt := time.Now()
	waitEOF(t, s, "the stream on the node that went silent", 40*time.Second)
	t.Logf("stream closed %s after A went silent", time.Since(frozeAt).Round(100*time.Millisecond))
	roundTrip(t, dialVia(t, gate, echo), "reconnect through B")
	if _, now, _, _ := sup.XrayState(); now != pid {
		t.Fatalf("xray was restarted (pid %d → %d)", pid, now)
	}
}

// waitEOF fails unless c is actually closed (EOF or reset) within timeout. A
// stalled stream only times out its reads, which waitClosed can't tell apart.
func waitEOF(t *testing.T, c net.Conn, what string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	buf := make([]byte, 64)
	for {
		_ = c.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
		_, err := c.Read(buf)
		var ne net.Error
		if err != nil && !(errors.As(err, &ne) && ne.Timeout()) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s: still open after %v", what, timeout)
		}
	}
}

// A manual pool against the real xray: a stream rides the pinned node; moving
// the pin to the other node applies live and closes the stream at once, and
// the reconnect rides the newly pinned node.
func TestRepinCutsOldNodesConnections(t *testing.T) {
	needRealXray(t)
	if os.Geteuid() != 0 {
		t.Skip("closing xray's sockets needs root")
	}
	useFreeXrayPorts(t)
	store, sup := newRealSupervisor(t)
	sup.probeURL = "http://" + startHTTP204(t) + "/generate_204"
	echo := startEcho(t)
	portA, servedA := startSocks5(t)
	portB, servedB := startSocks5(t)
	sub := &xray.Subscription{Name: "man", URL: "http://127.0.0.1/unused", Enabled: true, Strategy: xray.StrategyManual}
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
	if err := store.SetNodePinned(sub.ID, sn[0].Fingerprint, true); err != nil {
		t.Fatal(err)
	}
	masterSrv, _ := startSocks5(t)
	if err := store.CreateEntry(&xray.XrayEntry{Name: "M", Enabled: true, Dialer: "xraysub:man", Outbound: socksMember(masterSrv)}); err != nil {
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
	if err := sup.waitAPIReady(10 * time.Second); err != nil {
		t.Fatal(err)
	}
	if sl := loadedSlot(sup); !sl.Manual() {
		t.Fatalf("pool = %+v, want manual", sl)
	}
	beforeB := servedB()
	s := dialVia(t, gate, echo)
	roundTrip(t, s, "on A")
	if servedA() == 0 || servedB() != beforeB {
		t.Fatal("stream did not ride the pinned node A")
	}

	if err := store.SetNodePinned(sub.ID, sn[1].Fingerprint, true); err != nil {
		t.Fatal(err)
	}
	if err := store.SetNodePinned(sub.ID, sn[0].Fingerprint, false); err != nil {
		t.Fatal(err)
	}
	repinned := time.Now()
	sup.SyncDialerMembers()
	waitEOF(t, s, "the stream on the unpinned node", 10*time.Second)
	t.Logf("stream closed %s after the repin", time.Since(repinned).Round(100*time.Millisecond))
	beforeB = servedB()
	roundTrip(t, dialVia(t, gate, echo), "reconnect")
	if servedB() <= beforeB {
		t.Fatal("reconnect did not ride the newly pinned node B")
	}
	if _, now, _, _ := sup.XrayState(); now != pid {
		t.Fatalf("xray was restarted (pid %d → %d)", pid, now)
	}
}
