package daemon

import (
	"context"
	"strings"
	"testing"

	emxv1 "github.com/ehsan200/em-xray/api/emxv1"
	"github.com/ehsan200/em-xray/core/xray"
)

// A master's dialer can name several subscriptions and entries; they merge
// into one pool, and EntrySetDialer edits that list (or clears it) on a stored
// entry.
func TestEntrySetDialerMergesRefs(t *testing.T) {
	store, sup := newTestSupervisor(t)
	srv := &Server{store: store, sup: sup}
	socks := func(ip string) string {
		return `{"protocol":"socks","settings":{"servers":[{"address":"` + ip + `","port":1080}]}}`
	}
	for i, name := range []string{"a", "b"} {
		sub := &xray.Subscription{Name: name, URL: "https://example.com/" + name, Enabled: true}
		if err := store.CreateSubscription(sub); err != nil {
			t.Fatal(err)
		}
		ip := "203.0.113." + itoa(10+i)
		if err := store.ReplaceNodes(sub.ID, []xray.SubNode{{Name: name + "1", Fingerprint: "fp-" + name, Outbound: socks(ip)}}); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.CreateEntry(&xray.XrayEntry{Name: "relay", Enabled: true, Outbound: socks("203.0.113.50")}); err != nil {
		t.Fatal(err)
	}
	m := &xray.XrayEntry{Name: "M", Enabled: true, Outbound: socks("198.51.100.1")}
	if err := store.CreateEntry(m); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	set := func(d string) (*emxv1.EntryReply, error) {
		return srv.EntrySetDialer(ctx, &emxv1.EntryDialerRequest{Id: uint32(m.ID), Dialer: d})
	}

	reply, err := set(" xraysub:a, xraysub:b ,xray:relay,xraysub:a")
	if err != nil {
		t.Fatal(err)
	}
	if want := "xraysub:a,xraysub:b,xray:relay"; reply.Entry.Dialer != want || !reply.Entry.IsMaster {
		t.Fatalf("dialer = %q master=%v, want %q", reply.Entry.Dialer, reply.Entry.IsMaster, want)
	}
	entries, _ := store.ListEntries()
	slots, err := sup.resolveDialerSlots(entries)
	if err != nil {
		t.Fatal(err)
	}
	if len(slots) != 1 || slots[0].Master != "M" || len(slots[0].Members) != 3 {
		t.Fatalf("slots = %+v, want one pool for M with 3 members (a, b, relay)", slots)
	}

	for _, bad := range []string{"xraysub:nope", "xray:M", "bogus:x"} {
		if _, err := set(bad); err == nil {
			t.Errorf("dialer %q accepted", bad)
		}
	}

	in := &xray.Inbound{Name: "in", Protocol: "socks", Port: 21080, Target: "master:M", Enabled: true}
	if err := store.CreateInbound(in); err != nil {
		t.Fatal(err)
	}
	if _, err := set(""); err == nil || !strings.Contains(err.Error(), "retarget") {
		t.Fatalf("cleared a dialer an inbound targets: %v", err)
	}
	// Drop the inbound (not retarget it): with nothing routable the reconcile
	// below doesn't start an xray child.
	if err := store.DeleteInbound(in.ID); err != nil {
		t.Fatal(err)
	}
	if reply, err = set(""); err != nil || reply.Entry.IsMaster {
		t.Fatalf("clear: %v master=%v", err, reply.GetEntry().GetIsMaster())
	}
}

// EntryBulkDialer validates every item (against the others' new dialers too)
// before writing any, then applies them together.
func TestEntryBulkDialer(t *testing.T) {
	store, sup := newTestSupervisor(t)
	srv := &Server{store: store, sup: sup}
	socks := `{"protocol":"socks","settings":{"servers":[{"address":"203.0.113.9","port":1080}]}}`
	ids := map[string]uint32{}
	for _, n := range []string{"A", "B", "C", "relay"} {
		e := &xray.XrayEntry{Name: n, Enabled: true, Outbound: socks}
		if err := store.CreateEntry(e); err != nil {
			t.Fatal(err)
		}
		ids[n] = uint32(e.ID)
	}
	ctx := context.Background()
	bulk := func(kv ...string) (*emxv1.EntryListReply, error) {
		req := &emxv1.EntryBulkDialerRequest{}
		for i := 0; i < len(kv); i += 2 {
			req.Items = append(req.Items, &emxv1.EntryDialerRequest{Id: ids[kv[i]], Dialer: kv[i+1]})
		}
		return srv.EntryBulkDialer(ctx, req)
	}
	dialerOf := func(n string) string {
		e, err := store.GetEntryByName(n)
		if err != nil {
			t.Fatal(err)
		}
		return e.Dialer
	}

	// A cycle only visible across two edited entries.
	if _, err := bulk("A", "xray:B", "B", "xray:A"); err == nil || !strings.Contains(err.Error(), "cycle") {
		t.Fatalf("cross-entry cycle accepted: %v", err)
	}
	// One bad item → nothing written.
	if _, err := bulk("A", "xray:relay", "B", "xraysub:missing"); err == nil {
		t.Fatal("bad item accepted")
	}
	if d := dialerOf("A"); d != "" {
		t.Fatalf("A written despite refused batch: %q", d)
	}

	reply, err := bulk("A", "xray:relay", "B", "xray:relay, xray:C", "C", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(reply.Entries) != 2 { // C unchanged
		t.Fatalf("changed = %+v, want A and B", reply.Entries)
	}
	if dialerOf("A") != "xray:relay" || dialerOf("B") != "xray:relay,xray:C" {
		t.Fatalf("stored A=%q B=%q", dialerOf("A"), dialerOf("B"))
	}
}
