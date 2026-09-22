package xray

import (
	"encoding/base64"
	"strings"
	"testing"
)

func TestNewInboundFromTemplateReality(t *testing.T) {
	in, err := NewInboundFromTemplate("myserver", "vless-reality", "master:M")
	if err != nil {
		t.Fatal(err)
	}
	if in.Protocol != "vless" || in.Security != "reality" || in.Flow != "xtls-rprx-vision" {
		t.Errorf("bad template application: %+v", in)
	}
	if in.UUID == "" {
		t.Error("uuid must be auto-generated")
	}
	if in.RealityPrivateKey == "" || in.RealityPublicKey == "" || in.RealityShortID == "" {
		t.Error("reality keys/shortId must be auto-generated")
	}
	if in.RealitySNI == "" || in.RealityDest == "" {
		t.Error("reality dest/sni defaults must be filled")
	}
}

func TestCaddyXHTTPTemplate(t *testing.T) {
	in, err := NewInboundFromTemplate("edge", "vless-caddy-xhttp", "direct")
	if err != nil {
		t.Fatal(err)
	}
	if !IsCaddyXHTTP(*in) || in.Port != 0 {
		t.Fatalf("not a socket-backed Caddy inbound: %+v", in)
	}
	if in.XHTTPMode != "packet-up" || !strings.HasPrefix(in.Path, "/") || !strings.HasSuffix(in.Path, "/") {
		t.Fatalf("bad XHTTP defaults: path=%q mode=%q", in.Path, in.XHTTPMode)
	}
	if in.ClientEmail != "edge@xhttp.local" {
		t.Fatalf("email = %q", in.ClientEmail)
	}
	link := ShareLink(*in, "x.example.com")
	for _, want := range []string{"@x.example.com:443", "security=tls", "sni=x.example.com", "type=xhttp", "mode=packet-up"} {
		if !strings.Contains(link, want) {
			t.Errorf("link missing %q: %s", want, link)
		}
	}
}

func TestMaterializeNormalizesCaddyPath(t *testing.T) {
	in := &Inbound{Name: "edge", Protocol: "vless", Network: "xhttp", Security: "none", Listen: "/tmp/edge.sock,0666", Path: "api", XHTTPMode: "packet-up"}
	if err := Materialize(in); err != nil {
		t.Fatal(err)
	}
	if in.Path != "/api/" {
		t.Fatalf("path = %q, want /api/", in.Path)
	}
}

func TestMaterializeIdempotent(t *testing.T) {
	in := &Inbound{Name: "x", Protocol: "vless", Security: "reality", UUID: "keep-me"}
	if err := Materialize(in); err != nil {
		t.Fatal(err)
	}
	priv := in.RealityPrivateKey
	if err := Materialize(in); err != nil {
		t.Fatal(err)
	}
	if in.UUID != "keep-me" {
		t.Error("existing uuid must be preserved")
	}
	if in.RealityPrivateKey != priv {
		t.Error("existing reality key must be preserved (idempotent)")
	}
}

func TestRealityKeysValid(t *testing.T) {
	k, err := NewRealityKeys()
	if err != nil {
		t.Fatal(err)
	}
	priv, err := base64.RawURLEncoding.DecodeString(k.PrivateKey)
	if err != nil || len(priv) != 32 {
		t.Errorf("private key not 32 raw-url bytes: %v len=%d", err, len(priv))
	}
	pub, err := base64.RawURLEncoding.DecodeString(k.PublicKey)
	if err != nil || len(pub) != 32 {
		t.Errorf("public key not 32 raw-url bytes: %v len=%d", err, len(pub))
	}
}

func TestUUIDFormat(t *testing.T) {
	u, err := NewUUID()
	if err != nil {
		t.Fatal(err)
	}
	if len(u) != 36 || strings.Count(u, "-") != 4 || u[14] != '4' {
		t.Errorf("bad uuid v4: %q", u)
	}
}

func TestShareLinkVLESSReality(t *testing.T) {
	in, _ := NewInboundFromTemplate("srv", "vless-reality", "direct")
	in.Port = 8443
	link := ShareLink(*in, "1.2.3.4")
	for _, want := range []string{"vless://", in.UUID, "@1.2.3.4:8443", "security=reality", "pbk=" + in.RealityPublicKey, "sid=" + in.RealityShortID, "flow=xtls-rprx-vision"} {
		if !strings.Contains(link, want) {
			t.Errorf("link missing %q:\n%s", want, link)
		}
	}
}

func TestShareLinkVMess(t *testing.T) {
	in, _ := NewInboundFromTemplate("vm", "vmess-ws", "direct")
	in.Port = 8080
	link := ShareLink(*in, "example.com")
	if !strings.HasPrefix(link, "vmess://") {
		t.Fatalf("not a vmess link: %s", link)
	}
	dec, ok := decodeB64(strings.TrimPrefix(link, "vmess://"))
	if !ok || !strings.Contains(string(dec), "example.com") {
		t.Errorf("vmess link payload bad: %s", dec)
	}
}

func TestSocksPublicTemplateGeneratesCreds(t *testing.T) {
	in, err := NewInboundFromTemplate("tg", "socks-public", "direct")
	if err != nil {
		t.Fatal(err)
	}
	if in.Protocol != "socks" || in.Listen != "0.0.0.0" {
		t.Fatalf("socks-public should be public socks, got %s/%s", in.Protocol, in.Listen)
	}
	if in.SocksUser == "" || in.Password == "" {
		t.Fatalf("public socks must auto-generate user/pass, got user=%q pass=%q", in.SocksUser, in.Password)
	}
}

func TestSocksLoopbackNoAuth(t *testing.T) {
	in, err := NewInboundFromTemplate("local", "socks", "direct")
	if err != nil {
		t.Fatal(err)
	}
	if in.Listen != "127.0.0.1" {
		t.Fatalf("loopback socks listen = %q", in.Listen)
	}
	if in.SocksUser != "" {
		t.Fatalf("loopback socks must stay no-auth, got user=%q", in.SocksUser)
	}
}

func TestShareLinkSocksWithAuth(t *testing.T) {
	in := Inbound{Name: "tg", Protocol: "socks", Port: 11800, SocksUser: "alice", Password: "secret"}
	link := ShareLink(in, "1.2.3.4")
	if !strings.HasPrefix(link, "socks://") {
		t.Fatalf("not a socks link: %s", link)
	}
	creds := strings.TrimSuffix(strings.TrimPrefix(link, "socks://"), "@1.2.3.4:11800#tg")
	dec, err := base64.StdEncoding.DecodeString(creds)
	if err != nil || string(dec) != "alice:secret" {
		t.Fatalf("socks userinfo bad: %q -> %q (%v)", creds, dec, err)
	}
}

func TestShareLinkSocksNoAuth(t *testing.T) {
	in := Inbound{Name: "local", Protocol: "socks", Port: 1080}
	link := ShareLink(in, "1.2.3.4")
	if link != "socks://1.2.3.4:1080#local" {
		t.Fatalf("no-auth socks link = %q", link)
	}
}

func TestTelegramSocksLink(t *testing.T) {
	in := Inbound{Name: "tg", Protocol: "socks", Port: 11800, SocksUser: "alice", Password: "secret"}
	link := TelegramSocksLink(in, "1.2.3.4")
	for _, want := range []string{"tg://socks?", "server=1.2.3.4", "port=11800", "user=alice", "pass=secret"} {
		if !strings.Contains(link, want) {
			t.Errorf("tg link missing %q:\n%s", want, link)
		}
	}
	// non-socks inbound has no telegram link
	if got := TelegramSocksLink(Inbound{Protocol: "vless"}, "1.2.3.4"); got != "" {
		t.Errorf("tg link for vless should be empty, got %q", got)
	}
}

func TestParseTarget(t *testing.T) {
	cases := map[string][2]string{
		"direct":     {TargetDirect, ""},
		"master:Foo": {KindMaster, "Foo"},
		"xray:Bar":   {KindXray, "Bar"},
	}
	for in, want := range cases {
		k, n, err := ParseTarget(in)
		if err != nil || k != want[0] || n != want[1] {
			t.Errorf("ParseTarget(%q) = %q,%q,%v", in, k, n, err)
		}
	}
	for _, bad := range []string{"bogus:x", "master:", "xray"} {
		if _, _, err := ParseTarget(bad); err == nil {
			t.Errorf("expected error for %q", bad)
		}
	}
}
