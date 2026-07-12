package daemon

import (
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/ehsan200/em-xray/core/xray"
	"github.com/ehsan200/em-xray/internal/paths"
	"github.com/ehsan200/em-xray/internal/xraybin"
)

// TestReconcileEndToEnd proves the P4 milestone: Generate → config.json →
// watchdog starts real xray → a per-entry SOCKS inbound routes through the
// entry's outbound. Uses a `freedom` outbound dialing a LOCAL httptest server,
// so it needs no external network.
func TestReconcileEndToEnd(t *testing.T) {
	// Opt-in only: this binds the fixed 11800-range SOCKS ports and would
	// collide with any xray already running on the host. Run with EMX_E2E=1.
	if os.Getenv("EMX_E2E") != "1" {
		t.Skip("set EMX_E2E=1 to run the live-xray e2e (binds 11800-range ports)")
	}
	if xraybin.Version() == "none" {
		t.Skip("no embedded xray: run scripts/fetch-xray.sh")
	}

	// Target the freedom outbound will dial.
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent) // 204
	}))
	defer target.Close()

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

	// A freedom entry + a socks inbound targeting it:
	// SOCKS in → out-direct (freedom) → the target.
	if err := store.CreateEntry(&xray.XrayEntry{
		Name:     "direct",
		Enabled:  true,
		Outbound: `{"protocol":"freedom","settings":{}}`,
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateInbound(&xray.Inbound{
		Name:     "sock",
		Enabled:  true,
		Protocol: "socks",
		Target:   "xray:direct",
	}); err != nil {
		t.Fatal(err)
	}

	sup := NewSupervisor(store, p, log.New(io.Discard, "", 0))
	if err := sup.Reconcile(); err != nil {
		t.Fatal(err)
	}
	defer sup.Stop()

	// Wait for the xray child to come up.
	if !waitRunning(sup, 10*time.Second) {
		t.Fatal("xray did not start")
	}

	// Find the allocated SOCKS inbound port.
	inbounds, _ := store.ListInbounds()
	port := inbounds[0].Port
	if port == 0 {
		t.Fatal("no socks port allocated")
	}

	// Request the local target THROUGH the xray SOCKS proxy.
	proxyURL, _ := url.Parse("socks5://127.0.0.1:" + itoa(port))
	client := &http.Client{
		Transport: &http.Transport{Proxy: http.ProxyURL(proxyURL)},
		Timeout:   3 * time.Second,
	}

	var lastErr error
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := client.Get(target.URL)
		if err != nil {
			lastErr = err
			time.Sleep(200 * time.Millisecond)
			continue
		}
		resp.Body.Close()
		if resp.StatusCode == http.StatusNoContent {
			return // success: full inbound→routing→outbound chain works
		}
		lastErr = err
	}
	t.Fatalf("request through xray SOCKS failed: %v", lastErr)
}

func waitRunning(sup *Supervisor, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if running, _, _, _ := sup.XrayState(); running {
			return true
		}
		time.Sleep(100 * time.Millisecond)
	}
	return false
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [8]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
