package daemon

import (
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ehsan200/em-xray/core/xray"
)

// One pool node xray refuses must not take the server down: it is left out of
// its pool with xray's reason recorded, and everything else runs. An entry
// xray refuses is not applied at all — the running config keeps serving.
func TestInvalidObjectsNeverTakeXrayDown(t *testing.T) {
	needRealXray(t)
	useFreeXrayPorts(t)
	store, sup := newRealSupervisor(t)
	echo := startEcho(t)
	good, _ := startSocks5(t)
	srv, _ := startSocks5(t)

	sub := &xray.Subscription{Name: "pool", URL: "http://127.0.0.1/unused", Enabled: true}
	if err := store.CreateSubscription(sub); err != nil {
		t.Fatal(err)
	}
	bad := `{"protocol":"vless","settings":{"vnext":[{"address":"1.2.3.4","port":443,"users":[{"id":"b831381d-6324-4d53-ad4f-8cda48b30811","encryption":"none"}]}]},"streamSettings":{"network":"nope"}}`
	var nodes []xray.SubNode
	for i, ob := range []string{bad, socksMember(good)} {
		fp, _ := xray.Fingerprint([]byte(ob))
		nodes = append(nodes, xray.SubNode{Name: "n" + strconv.Itoa(i), Fingerprint: fp, Outbound: ob})
	}
	if err := store.ReplaceNodes(sub.ID, nodes); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateEntry(&xray.XrayEntry{Name: "M", Enabled: true, Dialer: "xraysub:pool", Outbound: socksMember(srv)}); err != nil {
		t.Fatal(err)
	}
	gate := freePort(t)
	if err := store.CreateInbound(&xray.Inbound{Name: "gate", Enabled: true, Protocol: "socks", Listen: "127.0.0.1", Port: gate, Target: "master:M"}); err != nil {
		t.Fatal(err)
	}
	if err := sup.Reconcile(); err != nil {
		t.Fatalf("a bad pool node failed the whole reconcile: %v", err)
	}
	waitListening(t, gate)
	roundTrip(t, dialVia(t, gate, echo), "through the good node")
	rej := sup.RejectedMembers()
	if len(rej) != 1 || !strings.Contains(rej[nodes[0].Fingerprint], "unknown transport protocol") {
		t.Fatalf("rejected = %v, want the bad node with xray's reason", rej)
	}
	if n := len(sup.loadedSlots[0].Members); n != 1 {
		t.Fatalf("pool = %d members, want 1", n)
	}
	if err := sup.waitAPIReady(10 * time.Second); err != nil {
		t.Fatal(err)
	}
	_, pid, _, _ := sup.XrayState()

	// A bad entry: refused, nothing applied, the running config keeps serving.
	if err := store.CreateEntry(&xray.XrayEntry{Name: "broken", Enabled: true, Outbound: bad}); err != nil {
		t.Fatal(err)
	}
	err := sup.Reconcile()
	if err == nil || !strings.Contains(err.Error(), "out-broken") || !strings.Contains(err.Error(), "unknown transport protocol") {
		t.Fatalf("reconcile err = %v, want the refused entry named with xray's reason", err)
	}
	if !strings.Contains(sup.ConfigError(), "out-broken") {
		t.Fatalf("ConfigError = %q", sup.ConfigError())
	}
	if _, now, _, _ := sup.XrayState(); now != pid {
		t.Fatal("xray was restarted onto a config it refuses")
	}
	roundTrip(t, dialVia(t, gate, echo), "still serving")
}
