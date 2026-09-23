package xray

import (
	"strings"
	"testing"
)

func TestMuxSupport(t *testing.T) {
	cases := []struct {
		ob   string
		ok   bool
		note string
	}{
		{`{"protocol":"vmess","streamSettings":{"network":"ws"}}`, true, ""},
		{`{"protocol":"trojan"}`, true, ""},
		{`{"protocol":"vless","settings":{"vnext":[{"users":[{"id":"x"}]}]}}`, true, ""},
		{`{"protocol":"vless","settings":{"vnext":[{"users":[{"id":"x","flow":"xtls-rprx-vision"}]}]}}`, false, "XTLS"},
		{`{"protocol":"vmess","streamSettings":{"network":"grpc"}}`, false, "already multiplexes"},
		{`{"protocol":"vless","streamSettings":{"network":"xhttp"}}`, false, "already multiplexes"},
		{`{"protocol":"shadowsocks"}`, false, "only VMess"},
		{`{"protocol":"vmess","mux":{"enabled":false}}`, false, "its own mux"},
		{`not json`, false, "does not parse"},
	}
	for _, c := range cases {
		ok, note := MuxSupport(c.ob)
		if ok != c.ok || !strings.Contains(note, c.note) {
			t.Errorf("MuxSupport(%s) = %v %q, want %v ~%q", c.ob, ok, note, c.ok, c.note)
		}
	}
}

// The mux block is added only when the entry opted in AND the outbound supports
// it; a user-written mux block is never touched.
func TestGenerateMux(t *testing.T) {
	entries := []XrayEntry{
		{Name: "on", Enabled: true, Mux: true, Outbound: `{"protocol":"vmess","streamSettings":{"network":"ws"}}`},
		{Name: "off", Enabled: true, Outbound: `{"protocol":"vmess","streamSettings":{"network":"ws"}}`},
		{Name: "grpc", Enabled: true, Mux: true, Outbound: `{"protocol":"vmess","streamSettings":{"network":"grpc"}}`},
		{Name: "own", Enabled: true, Mux: true, Outbound: `{"protocol":"vmess","mux":{"enabled":true,"concurrency":2}}`},
	}
	m := om(t, mustGen(t, entries, nil))
	byTag := map[string]map[string]any{}
	for _, o := range dig(t, m, "outbounds").([]any) {
		ob := o.(map[string]any)
		byTag[ob["tag"].(string)] = ob
	}
	if got := dig(t, byTag["out-on"], "mux", "xudpProxyUDP443"); got != "skip" {
		t.Errorf("out-on mux = %v", byTag["out-on"]["mux"])
	}
	for _, tag := range []string{"out-off", "out-grpc"} {
		if _, ok := byTag[tag]["mux"]; ok {
			t.Errorf("%s got a mux block", tag)
		}
	}
	if got := dig(t, byTag["out-own"], "mux", "concurrency"); got != float64(2) {
		t.Errorf("user mux block rewritten: %v", byTag["out-own"]["mux"])
	}
}
