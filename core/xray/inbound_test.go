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
