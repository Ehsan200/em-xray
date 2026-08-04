package daemon

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/ehsan200/em-xray/core/xray"
	"github.com/ehsan200/em-xray/internal/xraybin"
)

// TestProbeThroughRealXray exercises the whole probe path with the embedded
// xray: a throwaway instance is started with a socks inbound per config, and the
// probe URL is fetched through it. The target is a local httptest server and the
// outbound is freedom, so the test needs no internet — it proves the plumbing
// (config → xray → socks → HTTP → latency), not any remote server.
func TestProbeThroughRealXray(t *testing.T) {
	if xraybin.Version() == "none" {
		t.Skip("no embedded xray: run scripts/fetch-xray.sh")
	}
	dir := t.TempDir()
	bin, err := xraybin.Extract(dir)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	items := []xray.ProbeItem{
		{Name: "reachable", Outbound: `{"protocol":"freedom"}`},
		// blackhole accepts the connection then drops it — a config that exists
		// but cannot carry traffic, which must come back as a failure, not a 0ms.
		{Name: "dead-end", Outbound: `{"protocol":"blackhole"}`},
	}
	got := xray.ProbeOutbounds(ctx, items, xray.ProbeOptions{
		Bin: bin, AssetDir: dir, WorkDir: dir, URL: srv.URL, Timeout: 5 * time.Second,
	})
	if len(got) != 2 {
		t.Fatalf("want 2 results, got %d", len(got))
	}
	if got[0].Err != nil {
		t.Fatalf("freedom outbound should reach the local server: %v", got[0].Err)
	}
	if got[0].LatencyMs <= 0 {
		t.Errorf("want a positive latency, got %dms", got[0].LatencyMs)
	}
	if got[0].Name != "reachable" {
		t.Errorf("results out of order: %q", got[0].Name)
	}
	if got[1].Err == nil {
		t.Errorf("blackhole outbound reported success (%dms)", got[1].LatencyMs)
	}
}

// TestProbeConfigValidatesWithXray checks that a probe config for every built-in
// template's share link is accepted by `xray -test` (validate only, binds
// nothing).
func TestProbeConfigValidatesWithXray(t *testing.T) {
	if xraybin.Version() == "none" {
		t.Skip("no embedded xray: run scripts/fetch-xray.sh")
	}
	dir := t.TempDir()
	bin, err := xraybin.Extract(dir)
	if err != nil {
		t.Fatal(err)
	}
	items := []xray.ProbeItem{}
	ports := []int{}
	port := 24000
	for _, tmpl := range xray.TemplateList() {
		in, err := xray.NewInboundFromTemplate("srv", tmpl.Name, "direct")
		if err != nil {
			t.Fatal(err)
		}
		in.Port, in.PublicHost = 23456, "203.0.113.7"
		link := xray.ShareLink(*in, "")
		if link == "" || strings.HasPrefix(link, "socks://") {
			continue // socks links describe a proxy client, not a probe-able outbound
		}
		parsed, err := xray.ParseLink(link)
		if err != nil {
			t.Fatalf("%s: parse own share link: %v", tmpl.Name, err)
		}
		// Self-signed TLS links carry allowInsecure, which xray >=26 rejects
		// outright (migrated to pinnedPeerCertSha256) — a pre-existing link/core
		// mismatch, unrelated to probing.
		if strings.Contains(string(parsed.Outbound), "allowInsecure") {
			continue
		}
		items = append(items, xray.ProbeItem{Name: tmpl.Name, Outbound: string(parsed.Outbound)})
		ports = append(ports, port)
		port++
	}
	cfg, err := xray.BuildProbeConfig(items, ports)
	if err != nil {
		t.Fatal(err)
	}
	path := dir + "/probe.json"
	if err := writeFileAtomic(path, cfg); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(bin, "-test", "-c", path)
	cmd.Env = append(cmd.Env, "XRAY_LOCATION_ASSET="+dir)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("xray rejected the probe config: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "Configuration OK") {
		t.Errorf("no 'Configuration OK':\n%s", out)
	}
}
