package daemon

import (
	"testing"

	"github.com/ehsan200/em-xray/core/xray"
)

// Renaming a pool member keeps its pool on the same slot index and carries
// the pool's pick and the member's score, so the rename changes tags but not
// which nodes carry the masters.
func TestRenamedRefKeepsPoolState(t *testing.T) {
	store, sup := newTestSupervisor(t)
	sock := `{"protocol":"socks","settings":{"servers":[{"address":"1.2.3.4","port":1}]}}`
	for _, e := range []xray.XrayEntry{
		{Name: "n1", Enabled: true, Outbound: sock},
		{Name: "n2", Enabled: true, Outbound: sock},
		{Name: "M", Enabled: true, Dialer: "xray:n1,xray:n2", Outbound: sock},
	} {
		e := e
		if err := store.CreateEntry(&e); err != nil {
			t.Fatal(err)
		}
	}
	resolve := func() xray.Slot {
		t.Helper()
		entries, _ := store.ListEntries()
		slots, err := sup.resolveDialerSlots(entries)
		if err != nil || len(slots) != 1 {
			t.Fatalf("slots = %+v, %v", slots, err)
		}
		return slots[0]
	}
	// Pin the pool to a non-lowest index so a fresh allocation would show.
	sl := resolve()
	sup.slotIdx = map[string]int{sl.Key: 3}
	sl = resolve()
	sup.picker.observe([]xray.Slot{sl}, map[string]nodeStatus{
		xray.SlotMemberTag(3, "xray-n1"): win(3, 0, 100),
		xray.SlotMemberTag(3, "xray-n2"): win(3, 0, 200),
	})
	sl = resolve()
	if sl.Index != 3 || len(sl.Picked) != 2 {
		t.Fatalf("setup: slot %+v", sl)
	}
	sup.loadedSlots = []xray.Slot{sl}

	n1, _ := store.GetEntryByName("n1")
	if err := store.RenameEntry(n1.ID, "fast"); err != nil {
		t.Fatal(err)
	}
	sup.RenamedRef(xray.RefXray, "n1", "fast")

	sl = resolve()
	if sl.Index != 3 {
		t.Errorf("renamed pool moved to slot %d, want 3", sl.Index)
	}
	if !contains(sl.Picked, "xray-fast") || !contains(sl.Picked, "xray-n2") {
		t.Errorf("pick after rename = %v, want xray-fast and xray-n2", sl.Picked)
	}
	if sup.picker.scores[xray.SlotMemberTag(3, "xray-fast")] == nil {
		t.Error("renamed member's score was dropped")
	}
}

// Against the real xray: renaming the master an inbound routes to, and a node
// in its pool, keeps the inbound working — nothing is left pointing at the
// old names — and is applied without a restart.
func TestRenameKeepsInboundWorking(t *testing.T) {
	needRealXray(t)
	useFreeXrayPorts(t)
	store, sup := newRealSupervisor(t)
	sup.probeURL = "http://" + startHTTP204(t) + "/generate_204"
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
	roundTrip(t, dialVia(t, gate, echo), "before")

	for _, r := range []struct{ old, name string }{{"M", "Master"}, {"node", "edge"}} {
		e, _ := store.GetEntryByName(r.old)
		if err := store.RenameEntry(e.ID, r.name); err != nil {
			t.Fatal(err)
		}
		sup.RenamedRef(xray.RefXray, r.old, r.name)
		if err := sup.Reconcile(); err != nil {
			t.Fatalf("rename %s: %v", r.old, err)
		}
		before := served()
		roundTrip(t, dialVia(t, gate, echo), "after renaming "+r.old)
		if served() == before {
			t.Fatalf("after renaming %s traffic did not go through the pool node", r.old)
		}
	}
	if restarts, _ := sup.Counters(); restarts != 0 {
		t.Fatalf("rename restarted xray %d times", restarts)
	}
}
