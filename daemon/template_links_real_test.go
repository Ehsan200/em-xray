package daemon

import (
	"context"
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/ehsan200/em-xray/core/xray"
	"github.com/ehsan200/em-xray/internal/xraybin"
)

// Every built-in template's own share link must actually work: the server side
// runs the template's inbound in a real xray, the link is parsed back into an
// outbound exactly as `emx entry add --link` would, and a second real xray
// fetches a local target through it. This is the path a client (or another emx
// box using this one as a master) takes, so a link the current xray core
// refuses — or a TLS setup a client can't verify — fails here.
func startTLS13(t *testing.T) string {
	t.Helper()
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	srv.TLS = &tls.Config{MinVersion: tls.VersionTLS13}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return srv.Listener.Addr().String()
}

func TestTemplateLinksCarryTraffic(t *testing.T) {
	needRealXray(t)
	useFreeXrayPorts(t)
	target := "http://" + startHTTP204(t) + "/generate_204"
	store, sup := newRealSupervisor(t)
	if os.Getenv("EMX_TEST_LOG") != "" {
		_ = store.SetSetting(xray.SettingLogLevel, "debug")
	}

	// REALITY borrows a real TLS 1.3 server's handshake (its dest) on every
	// connection; point it at a local one so the test needs no internet.
	realityDest := startTLS13(t)

	type tc struct{ name, link string }
	var cases []tc
	for _, tmpl := range xray.TemplateList() {
		in, err := xray.NewInboundFromTemplate("t-"+tmpl.Name, tmpl.Name, "direct")
		if err != nil {
			t.Fatal(err)
		}
		if xray.IsUnixInbound(*in) || in.Protocol == "socks" {
			continue // caddy-fronted / socks: not a dialable server link here
		}
		in.Listen, in.Port = "127.0.0.1", freePort(t)
		if in.Security == "reality" {
			in.RealityDest = realityDest
		}
		if err := store.CreateInbound(in); err != nil {
			t.Fatal(err)
		}
		cases = append(cases, tc{tmpl.Name, xray.ShareLink(*in, "127.0.0.1")})
	}
	if err := sup.Reconcile(); err != nil {
		t.Fatal(err)
	}
	if !waitRunning(sup, 10*time.Second) {
		t.Fatal("server xray did not start")
	}
	if err := sup.waitAPIReady(10 * time.Second); err != nil {
		_, _, _, lastErr := sup.XrayState()
		t.Fatalf("server xray not up: %v (%s)", err, lastErr)
	}

	dir := t.TempDir()
	bin, err := xraybin.Extract(dir)
	if err != nil {
		t.Fatal(err)
	}
	var items []xray.ProbeItem
	for _, c := range cases {
		pl, err := xray.ParseLink(c.link)
		if err != nil {
			t.Errorf("%s: parse own share link: %v\n%s", c.name, err, c.link)
			continue
		}
		items = append(items, xray.ProbeItem{Name: c.name, Outbound: string(pl.Outbound)})
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if os.Getenv("EMX_TEST_LOG") != "" {
		defer func() {
			b, _ := os.ReadFile(sup.paths.ErrorLog())
			t.Logf("server error log:\n%s", b)
		}()
	}
	for _, r := range xray.ProbeOutbounds(ctx, items, xray.ProbeOptions{
		Bin: bin, AssetDir: dir, WorkDir: dir, URL: target, Timeout: 5 * time.Second,
	}) {
		if r.Err != nil {
			link := ""
			for _, c := range cases {
				if c.name == r.Name {
					link = c.link
				}
			}
			t.Errorf("%s: link does not carry traffic: %v\n  %s", r.Name, r.Err, strings.SplitN(link, "#", 2)[0])
		}
	}
}
