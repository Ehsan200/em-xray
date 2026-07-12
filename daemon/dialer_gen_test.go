package daemon

import (
	"io"
	"log"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gravisun/em-xray/core/xray"
	"github.com/gravisun/em-xray/internal/paths"
	"github.com/gravisun/em-xray/internal/xraybin"
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
