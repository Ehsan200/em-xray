package daemon

import (
	"encoding/json"
	"io"
	"log"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ehsan200/em-xray/core/xray"
	"github.com/ehsan200/em-xray/internal/paths"
	"github.com/ehsan200/em-xray/internal/xraybin"
)

// TestDialerConfigValidatesWithXray builds a full master-dialer config (api +
// slot + balancer + observatory + dialerProxy) from real store state and runs
// `xray -test` on it (validate-only, binds nothing).
func TestDialerConfigValidatesWithXray(t *testing.T) {
	if xraybin.Version() == "none" {
		t.Skip("no embedded xray: run scripts/fetch-xray.sh")
	}
	dir := t.TempDir()
	p := paths.Paths{Data: dir, Config: dir, State: dir, Cache: dir, Runtime: dir}
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	store, err := xray.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	// A pool member entry, a master dialing through it, and an inbound to the master.
	if err := store.CreateEntry(&xray.XrayEntry{Name: "node1", Enabled: true, Outbound: `{"protocol":"freedom","settings":{}}`}); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateEntry(&xray.XrayEntry{Name: "M", Enabled: true, Dialer: "xray:node1", Outbound: `{"protocol":"freedom","settings":{}}`}); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateInbound(&xray.Inbound{Name: "gate", Enabled: true, Protocol: "socks", Target: "master:M"}); err != nil {
		t.Fatal(err)
	}

	sup := NewSupervisor(store, p, log.New(io.Discard, "", 0))
	entries, _ := store.ListEntries()
	inbounds, _ := store.ListInbounds()
	slots, err := sup.resolveDialerSlots(entries)
	if err != nil {
		t.Fatal(err)
	}
	if len(slots) != 1 || len(slots[0].Members) != 1 {
		t.Fatalf("expected 1 slot with 1 member, got %+v", slots)
	}

	cfg, err := xray.Generate(entries, inbounds, slots, xray.GenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "dialer.json")
	if err := writeFileAtomic(path, cfg); err != nil {
		t.Fatal(err)
	}
	bin, err := xraybin.Extract(dir)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(bin, "-test", "-c", path)
	cmd.Env = append(cmd.Env, "XRAY_LOCATION_ASSET="+dir)
	out, err := cmd.CombinedOutput()
	if err != nil || !strings.Contains(string(out), "Configuration OK") {
		t.Fatalf("xray rejected dialer config: %v\n%s", err, out)
	}
}

// TestEmptyPoolMasterFailsClosed pins the no-leak invariant for a master whose
// dialer resolves to nothing (subscription not fetched yet, every node inactive,
// a ref pointing at something that no longer exists). Such a master must KEEP
// its slot: dropping it would strip the dialerProxy hop and let the master dial
// straight off this box, so an inbound aimed through the pool would egress from
// the server's own IP the moment the pool went empty. With the slot retained the
// traffic reaches a balancer that can pick nothing and falls back to `block`.
func TestEmptyPoolMasterFailsClosed(t *testing.T) {
	if xraybin.Version() == "none" {
		t.Skip("no embedded xray: run scripts/fetch-xray.sh")
	}
	dir := t.TempDir()
	p := paths.Paths{Data: dir, Config: dir, State: dir, Cache: dir, Runtime: dir}
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	store, err := xray.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	// Master points at a subscription that does not exist → zero members.
	if err := store.CreateEntry(&xray.XrayEntry{
		Name: "M", Enabled: true, Dialer: "xraysub:nope",
		Outbound: `{"protocol":"freedom","settings":{}}`,
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateInbound(&xray.Inbound{
		Name: "gate", Enabled: true, Protocol: "socks", Target: "master:M",
	}); err != nil {
		t.Fatal(err)
	}

	sup := NewSupervisor(store, p, log.New(io.Discard, "", 0))
	entries, _ := store.ListEntries()
	inbounds, _ := store.ListInbounds()
	slots, err := sup.resolveDialerSlots(entries)
	if err != nil {
		t.Fatal(err)
	}
	if len(slots) != 1 || len(slots[0].Members) != 0 {
		t.Fatalf("empty pool must still yield one memberless slot, got %+v", slots)
	}

	cfg, err := xray.Generate(entries, inbounds, slots, xray.GenOptions{})
	if err != nil {
		t.Fatal(err)
	}

	var m struct {
		Outbounds []struct {
			Tag            string `json:"tag"`
			StreamSettings struct {
				Sockopt struct {
					DialerProxy string `json:"dialerProxy"`
				} `json:"sockopt"`
			} `json:"streamSettings"`
		} `json:"outbounds"`
		Routing struct {
			Balancers []struct {
				Tag         string `json:"tag"`
				FallbackTag string `json:"fallbackTag"`
			} `json:"balancers"`
		} `json:"routing"`
	}
	if err := json.Unmarshal(cfg, &m); err != nil {
		t.Fatal(err)
	}
	if m.Outbounds[0].Tag != "block" {
		t.Errorf("outbounds[0] = %q; xray's default handler must be the blackhole", m.Outbounds[0].Tag)
	}
	var master struct{ found bool }
	for _, ob := range m.Outbounds {
		if ob.Tag != "out-M" {
			continue
		}
		master.found = true
		if ob.StreamSettings.Sockopt.DialerProxy != "dialer-M" {
			t.Errorf("out-M lost its dialerProxy (%q) — it would dial off this box",
				ob.StreamSettings.Sockopt.DialerProxy)
		}
	}
	if !master.found {
		t.Fatal("out-M missing from outbounds")
	}
	if len(m.Routing.Balancers) != 1 || m.Routing.Balancers[0].FallbackTag != "block" {
		t.Errorf("balancer must fall back to block, got %+v", m.Routing.Balancers)
	}

	path := filepath.Join(dir, "dialer-empty.json")
	if err := writeFileAtomic(path, cfg); err != nil {
		t.Fatal(err)
	}
	bin, err := xraybin.Extract(dir)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(bin, "-test", "-c", path)
	cmd.Env = append(cmd.Env, "XRAY_LOCATION_ASSET="+dir)
	out, err := cmd.CombinedOutput()
	if err != nil || !strings.Contains(string(out), "Configuration OK") {
		t.Fatalf("xray rejected memberless-slot config: %v\n%s", err, out)
	}
}
