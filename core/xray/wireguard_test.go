package xray

import (
	"strings"
	"testing"
)

func TestWireGuardOutboundFamilies(t *testing.T) {
	priv, pub, err := NewWireGuardKey()
	if err != nil {
		t.Fatal(err)
	}
	for addrs, want := range map[string]string{
		"10.0.0.2":             "ForceIPv4",
		"fd00::2/128":          "ForceIPv6",
		"10.0.0.2,fd00::2/128": "ForceIPv4v6",
	} {
		ob, err := WireGuard{PrivateKey: priv, PeerPublicKey: pub, Endpoint: "1.2.3.4:2408",
			Addresses: strings.Split(addrs, ",")}.Outbound()
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(ob), `"domainStrategy":"`+want+`"`) {
			t.Errorf("%s: want %s in %s", addrs, want, ob)
		}
	}
}

func TestWireGuardValidate(t *testing.T) {
	priv, pub, _ := NewWireGuardKey()
	ok := WireGuard{PrivateKey: priv, PeerPublicKey: pub, Endpoint: "1.2.3.4:2408", Addresses: []string{"10.0.0.2"}}
	if err := ok.Validate(); err != nil {
		t.Fatal(err)
	}
	bad := map[string]func(*WireGuard){
		"short key":     func(w *WireGuard) { w.PrivateKey = "abc" },
		"no address":    func(w *WireGuard) { w.Addresses = nil },
		"bad address":   func(w *WireGuard) { w.Addresses = []string{"nope"} },
		"no port":       func(w *WireGuard) { w.Endpoint = "1.2.3.4" },
		"reserved len":  func(w *WireGuard) { w.Reserved = []int{1, 2} },
		"reserved byte": func(w *WireGuard) { w.Reserved = []int{1, 2, 300} },
	}
	for name, mut := range bad {
		w := ok
		mut(&w)
		if w.Validate() == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
