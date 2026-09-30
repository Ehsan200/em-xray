package daemon

import (
	"context"
	"crypto/tls"
	"encoding/json"
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

// socksOutbound is the client side of a socks inbound. ParseLink has no socks://
// support (clients import those directly), so build what xray would use.
func socksOutbound(in xray.Inbound) string {
	srv := map[string]any{"address": "127.0.0.1", "port": in.Port}
	if in.SocksUser != "" {
		srv["users"] = []any{map[string]any{"user": in.SocksUser, "pass": in.Password}}
	}
	b, _ := json.Marshal(map[string]any{
		"protocol": "socks",
		"settings": map[string]any{"servers": []any{srv}},
	})
	return string(b)
}

func TestTemplateLinksCarryTraffic(t *testing.T) {
	needRealXray(t)
	useFreeXrayPorts(t)
	target := "http://" + startHTTP204(t) + "/generate_204"
	store, sup := newRealSupervisor(t)
	if os.Getenv("EMX_TEST_LOG") != "" {
		_ = store.SetSetting(xray.SettingLogLevel, "debug")
	}
	// Self-signed certs come from `xray tls cert`, as in the running daemon.
	oldCert := xray.TLSCertFunc
	installTLSCertGen(sup.paths)
	t.Cleanup(func() { xray.TLSCertFunc = oldCert })

	// REALITY borrows a real TLS 1.3 server's handshake (its dest) on every
	// connection; point it at a local one so the test needs no internet.
	realityDest := startTLS13(t)

	// Each template is checked twice where it supports users: its primary link
	// and the link of an extra user added afterwards (`emx in user add`).
	type tc struct{ name, link, outbound string }
	var cases []tc
	for _, tmpl := range xray.TemplateList() {
		in, err := xray.NewInboundFromTemplate("t-"+tmpl.Name, tmpl.Name, "direct")
		if err != nil {
			t.Fatalf("%s: create: %v", tmpl.Name, err)
		}
		if xray.IsUnixInbound(*in) {
			continue // caddy-fronted: public side is Caddy, not dialable here
		}
		in.Listen, in.Port = "127.0.0.1", freePort(t)
		if in.Security == "reality" {
			in.RealityDest = realityDest
		}
		if err := store.CreateInbound(in); err != nil {
			t.Fatalf("%s: store: %v", tmpl.Name, err)
		}
		if in.Protocol == "socks" {
			cases = append(cases, tc{name: tmpl.Name, outbound: socksOutbound(*in)})
			continue
		}
		cases = append(cases, tc{name: tmpl.Name, link: xray.ShareLink(*in, "127.0.0.1")})
		u, err := xray.NewInboundUser(in, "extra", 0)
		if err != nil {
			t.Fatalf("%s: new user: %v", tmpl.Name, err)
		}
		if err := store.CreateInboundUser(u); err != nil {
			t.Fatalf("%s: store user: %v", tmpl.Name, err)
		}
		cases = append(cases, tc{name: tmpl.Name + "/user", link: xray.ShareLinkForUser(*in, "127.0.0.1", *u)})
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
	for i, c := range cases {
		if c.outbound == "" {
			pl, err := xray.ParseLink(c.link)
			if err != nil {
				t.Errorf("%s: parse own share link: %v\n%s", c.name, err, c.link)
				continue
			}
			cases[i].outbound = string(pl.Outbound)
		}
		items = append(items, xray.ProbeItem{Name: c.name, Outbound: cases[i].outbound})
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if os.Getenv("EMX_TEST_LOG") != "" {
		defer func() {
			b, _ := os.ReadFile(sup.paths.ErrorLog())
			t.Logf("server error log:\n%s", b)
		}()
	}
	got := map[string]xray.ProbeResult{}
	for _, r := range xray.ProbeOutbounds(ctx, items, xray.ProbeOptions{
		Bin: bin, AssetDir: dir, WorkDir: dir, URL: target, Timeout: 5 * time.Second,
	}) {
		got[r.Name] = r
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r, ok := got[c.name]
			switch {
			case !ok:
				t.Fatal("never probed")
			case r.Err != nil:
				t.Fatalf("does not carry traffic: %v\n  %s", r.Err, strings.SplitN(c.link, "#", 2)[0])
			}
			t.Logf("ok %dms", r.LatencyMs)
		})
	}
}
