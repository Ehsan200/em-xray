package daemon

import (
	"context"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	emxv1 "github.com/ehsan200/em-xray/api/emxv1"
	"github.com/ehsan200/em-xray/core/xray"
	xproxy "golang.org/x/net/proxy"
)

// ifaceNames returns this box's loopback interface and one other interface
// that is up with an IPv4 address ("" when there is none).
func ifaceNames(t *testing.T) (loopback, other string) {
	t.Helper()
	ifs, err := net.Interfaces()
	if err != nil {
		t.Fatal(err)
	}
	for _, ifc := range ifs {
		switch {
		case ifc.Flags&net.FlagLoopback != 0:
			if loopback == "" {
				loopback = ifc.Name
			}
		case ifc.Flags&net.FlagUp != 0 && other == "":
			addrs, _ := ifc.Addrs()
			for _, a := range addrs {
				if ipn, ok := a.(*net.IPNet); ok && ipn.IP.To4() != nil {
					other = ifc.Name
					break
				}
			}
		}
	}
	return loopback, other
}

// Target checks run before anything is stored.
func TestCheckTarget(t *testing.T) {
	store, sup := newTestSupervisor(t)
	srv := &Server{store: store, sup: sup}
	lo, _ := ifaceNames(t)
	if got, err := srv.checkTarget("iface:" + lo); err != nil || got != "iface:"+lo {
		t.Fatalf("iface:%s → %q, %v", lo, got, err)
	}
	if _, err := srv.checkTarget("iface:nosuchif0"); err == nil || !strings.Contains(err.Error(), lo) {
		t.Fatalf("missing interface: %v, want an error listing the interfaces", err)
	}
	if err := store.CreateEntry(&xray.XrayEntry{Name: "plain", Enabled: true, Outbound: `{"protocol":"freedom"}`}); err != nil {
		t.Fatal(err)
	}
	if _, err := srv.checkTarget("master:plain"); err == nil {
		t.Fatal("master: target on an entry with no dialer accepted")
	}
	if _, err := srv.checkTarget("xray:ghost"); err == nil {
		t.Fatal("target naming no entry accepted")
	}
	if got, _ := srv.checkTarget(""); got != xray.TargetDirect {
		t.Fatalf("blank → %q, want direct", got)
	}
}

// Against the real xray: an inbound targeting the loopback interface reaches
// a loopback server; retargeted (live) to a non-loopback interface, its
// sockets are bound there and the loopback server becomes unreachable —
// proof the binding is applied, not ignored.
func TestIfaceTargetBindsOnRealXray(t *testing.T) {
	needRealXray(t)
	useFreeXrayPorts(t)
	lo, other := ifaceNames(t)
	if lo == "" {
		t.Skip("no loopback interface")
	}
	store, sup := newRealSupervisor(t)
	srv := &Server{store: store, sup: sup}
	echo := startEcho(t)
	gate := freePort(t)
	if err := store.CreateInbound(&xray.Inbound{Name: "gate", Enabled: true, Protocol: "socks", Listen: "127.0.0.1", Port: gate, Target: "iface:" + lo}); err != nil {
		t.Fatal(err)
	}
	if err := sup.Reconcile(); err != nil {
		t.Fatal(err)
	}
	waitListening(t, gate)
	if !waitRunning(sup, 10*time.Second) {
		t.Fatal("xray did not start")
	}
	roundTrip(t, dialVia(t, gate, echo), "through "+lo)
	if other == "" {
		t.Skip("no non-loopback interface to prove the binding")
	}
	if err := sup.waitAPIReady(10 * time.Second); err != nil {
		t.Fatal(err)
	}
	_, pid, _, _ := sup.XrayState()
	in, _ := store.GetInboundByName("gate")
	if _, err := srv.InboundSetTarget(context.Background(), &emxv1.InboundTargetRequest{Id: uint32(in.ID), Target: "iface:" + other}); err != nil {
		t.Fatal(err)
	}
	if _, now, _, _ := sup.XrayState(); now != pid {
		t.Fatalf("retarget restarted xray (pid %d → %d)", pid, now)
	}
	d, err := xproxy.SOCKS5("tcp", "127.0.0.1:"+strconv.Itoa(gate), nil, xproxy.Direct)
	if err != nil {
		t.Fatal(err)
	}
	c, err := d.Dial("tcp", echo)
	if err != nil {
		return // refused at the socks handshake: unreachable from that interface
	}
	defer c.Close()
	if err := tryRoundTrip(c, "through "+other); err == nil {
		t.Fatalf("reached a loopback server through %s: the interface binding was not applied", other)
	}
}
