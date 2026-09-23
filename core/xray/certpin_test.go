package xray

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/pem"
	"strings"
	"testing"
)

// xray v26 removed allowInsecure: links map pinning to pinnedPeerCertSha256 /
// verifyPeerCertByName and never emit allowInsecure, whatever the link says.
func TestParseLinkCertPinning(t *testing.T) {
	cases := []struct {
		link    string
		wantPin string
		wantVCN string
	}{
		{"vless://id@h:443?security=tls&allowInsecure=1#a", "", ""},
		{"vless://id@h:443?security=tls&allowInsecure=1&pcs=AB:CD:ef#a", "abcdef", ""},
		{"trojan://pw@h:443?security=tls&pcs=aa,BB&vcn=example.com#a", "aa,bb", "example.com"},
		{"hysteria2://auth@h:443/?insecure=1&pinSHA256=AA:BB#a", "aabb", ""},
		{"hysteria2://auth@h:443/?insecure=1#a", "", ""},
	}
	for _, c := range cases {
		pl, err := ParseLink(c.link)
		if err != nil {
			t.Fatalf("%s: %v", c.link, err)
		}
		ob := string(pl.Outbound)
		if strings.Contains(ob, "allowInsecure") {
			t.Errorf("%s: outbound carries allowInsecure: %s", c.link, ob)
		}
		m := om(t, pl.Outbound)
		tls, _ := dig(t, m, "streamSettings", "tlsSettings").(map[string]any)
		if got, _ := tls["pinnedPeerCertSha256"].(string); got != c.wantPin {
			t.Errorf("%s: pin = %q, want %q", c.link, got, c.wantPin)
		}
		if got, _ := tls["verifyPeerCertByName"].(string); got != c.wantVCN {
			t.Errorf("%s: vcn = %q, want %q", c.link, got, c.wantVCN)
		}
	}
}

// Stored entries/nodes from older builds carry allowInsecure; Generate must
// strip it (xray v26 refuses the whole config otherwise).
func TestGenerateHealsAllowInsecure(t *testing.T) {
	ob := `{"protocol":"trojan","settings":{"servers":[]},"streamSettings":{"security":"tls","tlsSettings":{"serverName":"x","allowInsecure":true}}}`
	entries := []XrayEntry{
		{Name: "e", Enabled: true, Outbound: ob},
		{Name: "M", Enabled: true, Dialer: "xraysub:s", Outbound: `{"protocol":"vmess"}`},
	}
	slots := []Slot{{Master: "M", Members: []SlotMember{{Key: "n", Outbound: ob}}}}
	b := mustGen(t, entries, slots)
	if strings.Contains(string(b), "allowInsecure") {
		t.Fatal("generated config still carries allowInsecure")
	}
}

// Self-signed TLS inbounds' links pin the certificate with the leaf SHA-256
// (what `xray tls hash` prints and pinnedPeerCertSha256 expects).
func TestShareLinkPinsSelfSignedCert(t *testing.T) {
	for _, tmpl := range []string{"vless-tls", "trojan-tls", "hysteria2", "vmess-tls-ws"} {
		in, err := NewInboundFromTemplate("s", tmpl, "direct")
		if err != nil {
			t.Fatal(err)
		}
		in.Port = 443
		block, _ := pem.Decode([]byte(in.TLSCert))
		sum := sha256.Sum256(block.Bytes)
		want := hex.EncodeToString(sum[:])
		if got := CertSHA256(in.TLSCert); got != want {
			t.Fatalf("CertSHA256 = %q, want %q", got, want)
		}
		pl, err := ParseLink(ShareLink(*in, "203.0.113.7"))
		if err != nil {
			t.Fatalf("%s: %v", tmpl, err)
		}
		m := om(t, pl.Outbound)
		if got := dig(t, m, "streamSettings", "tlsSettings", "pinnedPeerCertSha256"); got != want {
			t.Errorf("%s: link round-trips pin %v, want %s", tmpl, got, want)
		}
	}
}

// Hysteria2 inbounds: clients live under settings.clients (xray v26 ignores
// "users"), and TLS offers ALPN h3 (hysteria2 clients offer nothing else).
func TestHysteriaInboundShape(t *testing.T) {
	in, err := NewInboundFromTemplate("h", "hysteria2", "direct")
	if err != nil {
		t.Fatal(err)
	}
	in.Port = 443
	m := genMap(t, nil, []Inbound{*in})
	ib := m["inbounds"].([]any)[0].(map[string]any)
	if got := dig(t, ib, "settings", "clients", 0, "auth"); got != in.HysteriaAuth {
		t.Errorf("settings.clients[0].auth = %v", got)
	}
	if got := dig(t, ib, "streamSettings", "tlsSettings", "alpn", 0); got != "h3" {
		t.Errorf("alpn = %v, want h3", got)
	}
}
