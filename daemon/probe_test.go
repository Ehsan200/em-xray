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
		// xray >=26 rejects a config carrying allowInsecure outright; links
		// pin the certificate instead and the parser must never emit it.
		if strings.Contains(string(parsed.Outbound), "allowInsecure") {
			t.Fatalf("%s: parsed outbound carries allowInsecure: %s", tmpl.Name, parsed.Outbound)
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

// `emx entry test` / `emx sub test` probe many configs in one call. Two
// hysteria configs for the same server — one with a wrong password — must each
// get their own verdict: xray's hysteria client would otherwise reuse one QUIC
// session for both and pass or fail them together.
func TestProbeHysteriaSameServerKeepsVerdictsApart(t *testing.T) {
	needRealXray(t)
	useFreeXrayPorts(t)
	target := "http://" + startHTTP204(t) + "/generate_204"
	store, sup := newRealSupervisor(t)
	in, err := xray.NewInboundFromTemplate("hy", "hysteria2", "direct")
	if err != nil {
		t.Fatal(err)
	}
	in.Listen, in.Port = "127.0.0.1", freePort(t)
	if err := store.CreateInbound(in); err != nil {
		t.Fatal(err)
	}
	if err := sup.Reconcile(); err != nil {
		t.Fatal(err)
	}
	if err := sup.waitAPIReady(10 * time.Second); err != nil {
		t.Fatal(err)
	}
	good := xray.ShareLink(*in, "127.0.0.1")
	bad := strings.Replace(good, in.HysteriaAuth, "wrongpass", 1)
	var items []xray.ProbeItem
	for _, l := range []struct{ name, link string }{{"good", good}, {"bad", bad}, {"good2", good}} {
		pl, err := xray.ParseLink(l.link)
		if err != nil {
			t.Fatal(err)
		}
		items = append(items, xray.ProbeItem{Name: l.name, Outbound: string(pl.Outbound)})
	}
	dir := t.TempDir()
	bin, err := xraybin.Extract(dir)
	if err != nil {
		t.Fatal(err)
	}
	for round := 0; round < 5; round++ {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		res := xray.ProbeOutbounds(ctx, items, xray.ProbeOptions{
			Bin: bin, AssetDir: dir, WorkDir: dir, URL: target, Timeout: 5 * time.Second,
		})
		cancel()
		for _, r := range res {
			if (r.Name == "bad") != (r.Err != nil) {
				t.Fatalf("round %d: %s got err=%v", round, r.Name, r.Err)
			}
		}
	}
}
