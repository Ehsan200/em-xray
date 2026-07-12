package xray

import (
	"encoding/base64"
	"encoding/json"
	"testing"
)

// om unmarshals an outbound into a generic map for path assertions.
func om(t *testing.T, raw json.RawMessage) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("unmarshal outbound: %v\n%s", err, raw)
	}
	return m
}

// dig walks nested maps/slices: string keys index maps, ints index slices.
func dig(t *testing.T, m any, path ...any) any {
	t.Helper()
	cur := m
	for _, p := range path {
		switch k := p.(type) {
		case string:
			mm, ok := cur.(map[string]any)
			if !ok {
				t.Fatalf("dig: not a map at %v (%T)", p, cur)
			}
			cur = mm[k]
		case int:
			ss, ok := cur.([]any)
			if !ok || k >= len(ss) {
				t.Fatalf("dig: not a slice/index oob at %v", p)
			}
			cur = ss[k]
		}
	}
	return cur
}

func TestParseVLESSRealityTCP(t *testing.T) {
	link := "vless://11111111-2222-3333-4444-555555555555@ex.com:443?encryption=none&security=reality&sni=www.microsoft.com&fp=chrome&pbk=PUBKEY&sid=0123&type=tcp&flow=xtls-rprx-vision#Node-A"
	pl, err := ParseLink(link)
	if err != nil {
		t.Fatal(err)
	}
	if pl.Name != "Node-A" {
		t.Errorf("name = %q", pl.Name)
	}
	m := om(t, pl.Outbound)
	if got := dig(t, m, "protocol"); got != "vless" {
		t.Errorf("protocol = %v", got)
	}
	if got := dig(t, m, "settings", "vnext", 0, "address"); got != "ex.com" {
		t.Errorf("address = %v", got)
	}
	if got := dig(t, m, "settings", "vnext", 0, "port"); got != float64(443) {
		t.Errorf("port = %v", got)
	}
	if got := dig(t, m, "settings", "vnext", 0, "users", 0, "flow"); got != "xtls-rprx-vision" {
		t.Errorf("flow = %v", got)
	}
	if got := dig(t, m, "streamSettings", "security"); got != "reality" {
		t.Errorf("security = %v", got)
	}
	if got := dig(t, m, "streamSettings", "realitySettings", "publicKey"); got != "PUBKEY" {
		t.Errorf("pbk = %v", got)
	}
	if got := dig(t, m, "streamSettings", "realitySettings", "shortId"); got != "0123" {
		t.Errorf("sid = %v", got)
	}
}

func TestParseVLESSWSTLS(t *testing.T) {
	link := "vless://uuid-x@ex.com:443?type=ws&security=tls&path=%2Fwspath&host=cdn.ex.com&sni=cdn.ex.com#WS"
	pl, err := ParseLink(link)
	if err != nil {
		t.Fatal(err)
	}
	m := om(t, pl.Outbound)
	if got := dig(t, m, "streamSettings", "network"); got != "ws" {
		t.Errorf("network = %v", got)
	}
	if got := dig(t, m, "streamSettings", "wsSettings", "path"); got != "/wspath" {
		t.Errorf("path = %v", got)
	}
	if got := dig(t, m, "streamSettings", "wsSettings", "headers", "Host"); got != "cdn.ex.com" {
		t.Errorf("host header = %v", got)
	}
	if got := dig(t, m, "streamSettings", "tlsSettings", "serverName"); got != "cdn.ex.com" {
		t.Errorf("sni = %v", got)
	}
}

func TestParseVLESSXHTTP(t *testing.T) {
	link := "vless://u@ex.com:443?type=xhttp&security=tls&path=%2Fx&host=h.ex.com&mode=auto#X"
	pl, err := ParseLink(link)
	if err != nil {
		t.Fatal(err)
	}
	m := om(t, pl.Outbound)
	if got := dig(t, m, "streamSettings", "network"); got != "xhttp" {
		t.Errorf("network = %v", got)
	}
	if got := dig(t, m, "streamSettings", "xhttpSettings", "mode"); got != "auto" {
		t.Errorf("mode = %v", got)
	}
}

func TestParseTrojanGRPC(t *testing.T) {
	link := "trojan://secretpass@ex.com:443?type=grpc&serviceName=mysvc&mode=multi&security=tls&sni=ex.com#T"
	pl, err := ParseLink(link)
	if err != nil {
		t.Fatal(err)
	}
	m := om(t, pl.Outbound)
	if got := dig(t, m, "protocol"); got != "trojan" {
		t.Errorf("protocol = %v", got)
	}
	if got := dig(t, m, "settings", "servers", 0, "password"); got != "secretpass" {
		t.Errorf("password = %v", got)
	}
	if got := dig(t, m, "streamSettings", "grpcSettings", "serviceName"); got != "mysvc" {
		t.Errorf("serviceName = %v", got)
	}
	if got := dig(t, m, "streamSettings", "grpcSettings", "multiMode"); got != true {
		t.Errorf("multiMode = %v", got)
	}
}

func TestParseTrojanDefaultsTLS(t *testing.T) {
	pl, err := ParseLink("trojan://p@ex.com:443#T2")
	if err != nil {
		t.Fatal(err)
	}
	m := om(t, pl.Outbound)
	if got := dig(t, m, "streamSettings", "security"); got != "tls" {
		t.Errorf("trojan should default to tls, got %v", got)
	}
}

func TestParseVMessWS(t *testing.T) {
	vm := map[string]any{
		"v": "2", "ps": "VM-Node", "add": "ex.com", "port": "443", "id": "uuid-v",
		"aid": "0", "scy": "auto", "net": "ws", "type": "none",
		"host": "cdn.ex.com", "path": "/vm", "tls": "tls", "sni": "cdn.ex.com",
	}
	raw, _ := json.Marshal(vm)
	link := "vmess://" + base64.StdEncoding.EncodeToString(raw)
	pl, err := ParseLink(link)
	if err != nil {
		t.Fatal(err)
	}
	if pl.Name != "VM-Node" {
		t.Errorf("name = %q", pl.Name)
	}
	m := om(t, pl.Outbound)
	if got := dig(t, m, "protocol"); got != "vmess" {
		t.Errorf("protocol = %v", got)
	}
	if got := dig(t, m, "settings", "vnext", 0, "port"); got != float64(443) {
		t.Errorf("port(string->int) = %v", got)
	}
	if got := dig(t, m, "streamSettings", "wsSettings", "path"); got != "/vm" {
		t.Errorf("path = %v", got)
	}
	if got := dig(t, m, "streamSettings", "security"); got != "tls" {
		t.Errorf("security = %v", got)
	}
}

func TestParseSS(t *testing.T) {
	// SIP002: userinfo base64(method:password)
	userinfo := base64.StdEncoding.EncodeToString([]byte("aes-256-gcm:secret"))
	pl, err := ParseLink("ss://" + userinfo + "@ex.com:8388#SS")
	if err != nil {
		t.Fatal(err)
	}
	m := om(t, pl.Outbound)
	if got := dig(t, m, "protocol"); got != "shadowsocks" {
		t.Errorf("protocol = %v", got)
	}
	if got := dig(t, m, "settings", "servers", 0, "method"); got != "aes-256-gcm" {
		t.Errorf("method = %v", got)
	}
	if got := dig(t, m, "settings", "servers", 0, "password"); got != "secret" {
		t.Errorf("password = %v", got)
	}

	// Legacy: whole userinfo+host base64
	whole := base64.StdEncoding.EncodeToString([]byte("chacha20-ietf-poly1305:pw@ex.com:8389"))
	pl2, err := ParseLink("ss://" + whole + "#SS2")
	if err != nil {
		t.Fatal(err)
	}
	m2 := om(t, pl2.Outbound)
	if got := dig(t, m2, "settings", "servers", 0, "port"); got != float64(8389) {
		t.Errorf("legacy port = %v", got)
	}
}

func TestParseLinkErrors(t *testing.T) {
	for _, bad := range []string{"", "http://x", "vless://@ex.com:443", "trojan://ex.com"} {
		if _, err := ParseLink(bad); err == nil {
			t.Errorf("expected error for %q", bad)
		}
	}
}

// Parsed outbounds must fingerprint stably (identity independent of the #name).
func TestParsedFingerprintIgnoresName(t *testing.T) {
	a, _ := ParseLink("vless://u@ex.com:443?type=ws&path=%2Fp#NameOne")
	b, _ := ParseLink("vless://u@ex.com:443?type=ws&path=%2Fp#NameTwo")
	fa, err := Fingerprint(a.Outbound)
	if err != nil {
		t.Fatal(err)
	}
	fb, _ := Fingerprint(b.Outbound)
	if fa != fb {
		t.Errorf("fingerprint must ignore display name: %s != %s", fa, fb)
	}
}
