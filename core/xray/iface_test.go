package xray

import (
	"reflect"
	"testing"
)

func TestParseIfaceTarget(t *testing.T) {
	for _, ok := range []string{"iface:eth0", "iface:wg0", "iface:enp3s0.100", "iface:tun@1"} {
		if k, _, err := ParseTarget(ok); err != nil || k != KindIface {
			t.Errorf("%s: kind %q err %v", ok, k, err)
		}
	}
	for _, bad := range []string{"iface:", "iface:has space", "iface:waytoolonginterface0", "iface:a/b"} {
		if _, _, err := ParseTarget(bad); err == nil {
			t.Errorf("%s accepted", bad)
		}
	}
}

// Each interface an enabled inbound targets gets one freedom outbound bound
// to it, and the inbound is routed there.
func TestGenerateIfaceOutbound(t *testing.T) {
	ins := []Inbound{
		{Name: "a", Enabled: true, Protocol: "socks", Listen: "127.0.0.1", Port: 1001, Target: "iface:wg0"},
		{Name: "b", Enabled: true, Protocol: "socks", Listen: "127.0.0.1", Port: 1002, Target: "iface:wg0"},
		{Name: "c", Enabled: true, Protocol: "socks", Listen: "127.0.0.1", Port: 1003, Target: "iface:eth1"},
		{Name: "off", Enabled: false, Protocol: "socks", Listen: "127.0.0.1", Port: 1004, Target: "iface:eth9"},
	}
	b, err := Generate(nil, ins, nil, GenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	m := om(t, b)
	ifaces := map[string]any{}
	for _, o := range dig(t, m, "outbounds").([]any) {
		ob := o.(map[string]any)
		if tag := ob["tag"].(string); len(tag) > 6 && tag[:6] == "iface-" {
			if ob["protocol"] != "freedom" {
				t.Errorf("%s protocol = %v", tag, ob["protocol"])
			}
			ifaces[tag] = dig(t, ob, "streamSettings", "sockopt", "interface")
		}
	}
	if want := map[string]any{"iface-wg0": "wg0", "iface-eth1": "eth1"}; !reflect.DeepEqual(ifaces, want) {
		t.Fatalf("iface outbounds = %v, want %v", ifaces, want)
	}
	routes := map[string]any{}
	for _, r := range dig(t, m, "routing", "rules").([]any) {
		rule := r.(map[string]any)
		if in, ok := rule["inboundTag"].([]any); ok && len(in) == 1 {
			routes[in[0].(string)] = rule["outboundTag"]
		}
	}
	if routes["in-a"] != "iface-wg0" || routes["in-b"] != "iface-wg0" || routes["in-c"] != "iface-eth1" {
		t.Fatalf("routes = %v", routes)
	}
}

// Renaming an entry never touches an iface target that happens to share its
// name.
func TestRenameEntryLeavesIfaceTargets(t *testing.T) {
	s := memStore(t)
	e := &XrayEntry{Name: "wg0", Enabled: true, Outbound: `{"protocol":"freedom"}`}
	if err := s.CreateEntry(e); err != nil {
		t.Fatal(err)
	}
	in := &Inbound{Name: "i", Enabled: true, Protocol: "socks", Target: "iface:wg0"}
	if err := s.CreateInbound(in); err != nil {
		t.Fatal(err)
	}
	if err := s.RenameEntry(e.ID, "other"); err != nil {
		t.Fatal(err)
	}
	got, _ := s.GetInbound(in.ID)
	if got.Target != "iface:wg0" {
		t.Fatalf("target = %q, want iface:wg0 untouched", got.Target)
	}
}
