package daemon

import (
	"reflect"
	"testing"

	"github.com/ehsan200/em-xray/core/xray"
)

func genLive(t *testing.T, entries []xray.XrayEntry, inbounds []xray.Inbound, slots []xray.Slot) liveConfig {
	t.Helper()
	raw, err := xray.Generate(entries, inbounds, slots, xray.GenOptions{})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	lc, err := parseLiveConfig(raw)
	if err != nil {
		t.Fatalf("parseLiveConfig: %v", err)
	}
	return lc
}

func liveEntry(name, host string) xray.XrayEntry {
	return xray.XrayEntry{Name: name, Enabled: true,
		Outbound: `{"protocol":"vmess","settings":{"vnext":[{"address":"` + host + `","port":443,"users":[{"id":"x"}]}]}}`}
}

func liveInbound(name string, port int, target, uuid string) xray.Inbound {
	return xray.Inbound{Name: name, Enabled: true, Protocol: "vless", Port: port, Target: target, UUID: uuid}
}

// Everything done day to day must apply without a restart and touch only what
// changed; revoking a client and changing a fixed section must restart.
func TestPlanLive(t *testing.T) {
	ents := []xray.XrayEntry{liveEntry("a", "a.example"), liveEntry("b", "b.example")}
	ins := []xray.Inbound{liveInbound("ia", 12001, "xray:a", "u1"), liveInbound("ib", 12002, "xray:b", "u2")}
	old := genLive(t, ents, ins, nil)

	t.Run("identical", func(t *testing.T) {
		if p := planLive(old, genLive(t, ents, ins, nil)); !p.empty() {
			t.Fatalf("plan = %+v, want empty", p)
		}
	})

	t.Run("entry edited", func(t *testing.T) {
		p := planLive(old, genLive(t, []xray.XrayEntry{liveEntry("a", "a2.example"), ents[1]}, ins, nil))
		want := []string{"out-a"}
		if p.restart || p.routing || !reflect.DeepEqual(p.rmOut, want) || !reflect.DeepEqual(p.addOut, want) || len(p.rmIn)+len(p.addIn) != 0 {
			t.Fatalf("plan = %+v, want only out-a replaced", p)
		}
	})

	t.Run("entry and inbound added", func(t *testing.T) {
		p := planLive(old, genLive(t, append(append([]xray.XrayEntry{}, ents...), liveEntry("c", "c.example")),
			append(append([]xray.Inbound{}, ins...), liveInbound("ic", 12003, "xray:c", "u3")), nil))
		if p.restart || !p.routing || len(p.rmOut)+len(p.rmIn) != 0 ||
			!reflect.DeepEqual(p.addOut, []string{"out-c"}) || !reflect.DeepEqual(p.addIn, []string{"in-ic"}) {
			t.Fatalf("plan = %+v, want c + ic added with routing", p)
		}
	})

	t.Run("entry sorting first stays live", func(t *testing.T) {
		// block is outbounds[0] whatever the entry names are.
		if p := planLive(old, genLive(t, append([]xray.XrayEntry{liveEntry("0first", "z.example")}, ents...), ins, nil)); p.restart {
			t.Fatalf("adding an entry forced a restart: %s", p.why)
		}
	})

	t.Run("retarget inbound", func(t *testing.T) {
		p := planLive(old, genLive(t, ents, []xray.Inbound{liveInbound("ia", 12001, "xray:b", "u1"), ins[1]}, nil))
		if p.restart || !p.routing || len(p.addIn)+len(p.rmIn)+len(p.addOut)+len(p.rmOut) != 0 {
			t.Fatalf("plan = %+v, want routing only", p)
		}
	})

	t.Run("client added", func(t *testing.T) {
		in := ins[0]
		in.Users = []xray.InboundUser{{UUID: "u9", Email: "ia.bob"}}
		p := planLive(old, genLive(t, ents, []xray.Inbound{in, ins[1]}, nil))
		if p.restart || !reflect.DeepEqual(p.addIn, []string{"in-ia"}) {
			t.Fatalf("plan = %+v, want in-ia replaced live", p)
		}
	})

	t.Run("client revoked restarts", func(t *testing.T) {
		p := planLive(old, genLive(t, ents, []xray.Inbound{liveInbound("ia", 12001, "xray:a", "rotated"), ins[1]}, nil))
		if !p.restart {
			t.Fatalf("revoked credential applied live — its sessions would survive")
		}
	})

	t.Run("inbound removed restarts", func(t *testing.T) {
		if p := planLive(old, genLive(t, ents, ins[:1], nil)); !p.restart {
			t.Fatalf("removing an authenticated inbound applied live — its sessions would survive")
		}
	})

	t.Run("master added, pool churn, alias", func(t *testing.T) {
		m := liveEntry("m", "m.example")
		m.Dialer = "xraysub:s"
		member := func(k string) xray.SlotMember {
			return xray.SlotMember{Key: k, Outbound: `{"protocol":"socks","settings":{"servers":[{"address":"1.1.1.1","port":1}]}}`}
		}
		with := append(append([]xray.XrayEntry{}, ents...), m)
		mIns := append(append([]xray.Inbound{}, ins...), liveInbound("im", 12009, "master:m", "u9"))
		now := genLive(t, with, mIns, []xray.Slot{{Master: "m", Members: []xray.SlotMember{member("n1")}}})
		if p := planLive(old, now); p.restart {
			t.Fatalf("first master forced a restart: %s", p.why)
		}
		later := genLive(t, with, mIns, []xray.Slot{{Master: "m", Members: []xray.SlotMember{member("n1"), member("n2")}}})
		p := planLive(now, later)
		if p.restart || !reflect.DeepEqual(p.addOut, []string{"slot0-out-n2"}) || len(p.rmOut) != 0 {
			t.Fatalf("pool growth plan = %+v, want one member added live", p)
		}
		m2 := liveEntry("m2", "m.example")
		m2.Dialer = "xraysub:s"
		alias := genLive(t, append(with, m2), mIns, []xray.Slot{{Master: "m", Aliases: []string{"m2"}, Members: []xray.SlotMember{member("n1"), member("n2")}}})
		p = planLive(later, alias)
		if p.restart || !reflect.DeepEqual(p.addOut, []string{"dialer-m2", "out-m2"}) || len(p.rmOut) != 0 {
			t.Fatalf("alias plan = %+v, want dialer-m2 + out-m2 added", p)
		}
	})

	t.Run("fixed section restarts", func(t *testing.T) {
		raw, _ := xray.Generate(ents, ins, nil, xray.GenOptions{LogLevel: "debug"})
		lc, _ := parseLiveConfig(raw)
		if p := planLive(old, lc); !p.restart {
			t.Fatal("log level change applied live")
		}
	})
}

// When a master's dialer outbound is replaced, the master goes down first and
// comes back last, so it never exists while its dialer doesn't (the no-leak
// guarantee must not depend on how xray treats a dangling dialerProxy).
func TestPlanLiveDialerSwapTakesMasterDown(t *testing.T) {
	m := liveEntry("m", "m.example")
	m.Dialer = "xraysub:s"
	ents := []xray.XrayEntry{m}
	member := xray.SlotMember{Key: "n1", Outbound: `{"protocol":"socks","settings":{"servers":[{"address":"1.1.1.1","port":1}]}}`}
	old := genLive(t, ents, nil, []xray.Slot{{Master: "m", Index: 0, Members: []xray.SlotMember{member}}})
	// The master moves to another slot: dialer-m now points at another port.
	want := genLive(t, ents, nil, []xray.Slot{{Master: "m", Index: 1, Members: []xray.SlotMember{member}}})
	p := planLive(old, want)
	if p.restart {
		t.Fatalf("restart: %s", p.why)
	}
	if !reflect.DeepEqual(p.rmOut, []string{"dialer-m", "out-m", "slot0-out-n1"}) {
		t.Fatalf("rmOut = %v; out-m must be replaced along with dialer-m", p.rmOut)
	}
	changed := intersect(p.rmOut, toSet(p.addOut))
	if got := p.dependentsFirst(changed); !reflect.DeepEqual(got, []string{"out-m", "dialer-m"}) {
		t.Fatalf("removal order = %v, want the master before its dialer", got)
	}
	if got := reversed(p.dependentsFirst(p.addOut)); got[len(got)-1] != "out-m" {
		t.Fatalf("add order = %v, want the master last", got)
	}
}

// Formatting and key order must never read as a change.
func TestParseLiveConfigCanonical(t *testing.T) {
	a := []byte(`{"log":{"loglevel":"warning"},"inbounds":[{"tag":"i","port":1}],"outbounds":[{"tag":"o","protocol":"freedom"}],"routing":{"rules":[]}}`)
	b := []byte(`{ "routing": {"rules": []}, "outbounds": [{"protocol":"freedom", "tag":"o"}], "inbounds":[{"port":1,"tag":"i"}], "log":{"loglevel":"warning"}}`)
	la, err := parseLiveConfig(a)
	if err != nil {
		t.Fatal(err)
	}
	lb, err := parseLiveConfig(b)
	if err != nil {
		t.Fatal(err)
	}
	if p := planLive(la, lb); !p.empty() {
		t.Fatalf("plan = %+v, want empty", p)
	}
	if _, err := parseLiveConfig([]byte(`{"outbounds":[{"tag":"o"},{"tag":"o"}]}`)); err == nil {
		t.Fatalf("duplicate tags must be rejected")
	}
}
