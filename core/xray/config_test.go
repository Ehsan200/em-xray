package xray

import (
	"bytes"
	"encoding/json"
	"testing"
)

func genMap(t *testing.T, entries []XrayEntry, inbounds []Inbound) map[string]any {
	t.Helper()
	b, err := Generate(entries, inbounds, nil, GenOptions{AccessLog: "/a", ErrorLog: "/e"})
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return m
}

func TestGenerateEntriesAndInbounds(t *testing.T) {
	entries := []XrayEntry{
		{Name: "beta", Enabled: true, Outbound: `{"protocol":"vless","settings":{}}`},
		{Name: "alpha", Enabled: true, Outbound: `{"protocol":"trojan","settings":{}}`},
		{Name: "off", Enabled: false, Outbound: `{"protocol":"vless","settings":{}}`},
	}
	inbounds := []Inbound{
		{Name: "sock", Enabled: true, Protocol: "socks", Port: 11800, Target: "xray:alpha"},
	}
	m := genMap(t, entries, inbounds)

	// in-sock plus the always-on api dokodemo inbound.
	ins := m["inbounds"].([]any)
	if len(ins) != 2 {
		t.Fatalf("inbounds = %d, want 2 (in-sock + api)", len(ins))
	}
	if tag := dig(t, ins, 0, "tag"); tag != "in-sock" {
		t.Errorf("inbound tag = %v", tag)
	}
	if tag := dig(t, ins, 1, "tag"); tag != ApiTag {
		t.Errorf("second inbound should be api, got %v", tag)
	}

	outs := m["outbounds"].([]any)
	// direct, block, out-alpha, out-beta (disabled 'off' excluded)
	if len(outs) != 4 {
		t.Fatalf("outbounds = %d, want 4", len(outs))
	}
	if dig(t, outs, 0, "tag") != "direct" || dig(t, outs, 1, "tag") != "block" {
		t.Errorf("direct/block must lead outbounds")
	}

	// api rule is prepended first; the user rule follows.
	rules := dig(t, m, "routing", "rules").([]any)
	if len(rules) != 2 {
		t.Fatalf("rules = %d, want 2 (api + in-sock)", len(rules))
	}
	if dig(t, rules, 0, "outboundTag") != ApiTag {
		t.Errorf("api rule must be first, got %v", rules)
	}
	if dig(t, rules, 1, "outboundTag") != "out-alpha" {
		t.Errorf("rule should route in-sock → out-alpha, got %v", rules)
	}

	// stats + traffic policy must always be present.
	if _, ok := m["stats"]; !ok {
		t.Error("stats must be enabled")
	}
	if dig(t, m, "policy", "system", "statsInboundUplink") != true {
		t.Error("inbound traffic stats policy must be on")
	}
}

func TestGenerateRejectsMissingTarget(t *testing.T) {
	inbounds := []Inbound{{Name: "x", Enabled: true, Protocol: "socks", Port: 11800, Target: "xray:ghost"}}
	if _, err := Generate(nil, inbounds, nil, GenOptions{}); err == nil {
		t.Error("inbound targeting a nonexistent entry should error")
	}
}

func TestGenerateDeterministic(t *testing.T) {
	a := []XrayEntry{
		{Name: "a", Enabled: true, Outbound: `{"protocol":"vless","settings":{}}`},
		{Name: "b", Enabled: true, Outbound: `{"protocol":"vless","settings":{}}`},
	}
	b := []XrayEntry{a[1], a[0]} // reversed input order
	g1, _ := Generate(a, nil, nil, GenOptions{})
	g2, _ := Generate(b, nil, nil, GenOptions{})
	if !bytes.Equal(g1, g2) {
		t.Error("generate must be order-independent (deterministic)")
	}
}

func TestHealXHTTPExtra(t *testing.T) {
	// extra stored as a JSON *string* → healed to an object.
	ob, err := entryOutbound(`{"protocol":"vless","streamSettings":{"network":"xhttp","xhttpSettings":{"extra":"{\"scMaxEachPostBytes\":1000000}"}}}`, "out-x")
	if err != nil {
		t.Fatal(err)
	}
	extra := dig(t, ob, "streamSettings", "xhttpSettings", "extra")
	if _, ok := extra.(map[string]any); !ok {
		t.Errorf("extra should be healed to object, got %T", extra)
	}

	// unparseable string → dropped so xray still starts.
	ob2, _ := entryOutbound(`{"protocol":"vless","streamSettings":{"network":"xhttp","xhttpSettings":{"extra":"not json"}}}`, "out-y")
	xs := dig(t, ob2, "streamSettings", "xhttpSettings").(map[string]any)
	if _, present := xs["extra"]; present {
		t.Error("unparseable extra should be dropped")
	}
}

func TestAssignInboundPorts(t *testing.T) {
	inbounds := []Inbound{
		{Name: "keep", Enabled: true, Port: 11805}, // pre-assigned, retained
		{Name: "new1", Enabled: true},
		{Name: "off", Enabled: false}, // disabled: no port
		{Name: "new2", Enabled: true},
	}
	changed := AssignInboundPorts(inbounds)
	if len(changed) != 2 {
		t.Fatalf("changed = %d, want 2", len(changed))
	}
	if inbounds[0].Port != 11805 {
		t.Error("pre-assigned port must be retained")
	}
	if inbounds[1].Port != 11800 || inbounds[3].Port != 11801 {
		t.Errorf("new ports = %d,%d; want lowest-free 11800,11801", inbounds[1].Port, inbounds[3].Port)
	}
	if inbounds[2].Port != 0 {
		t.Error("disabled inbound must not get a port")
	}
}
