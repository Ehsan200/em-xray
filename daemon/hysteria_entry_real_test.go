package daemon

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/ehsan200/em-xray/core/xray"
	"github.com/ehsan200/em-xray/internal/paths"
	"github.com/ehsan200/em-xray/internal/xraybin"
	xproxy "golang.org/x/net/proxy"
)

// Two boxes: server A runs the hysteria2 template; server B (a full emx
// supervisor) takes A's share link as an entry and uses it every way an entry
// can be used (a hysteria master excepted: see below). A's inbound egresses through a counting SOCKS stand-in, so a
// connection only counts when it really crossed the hysteria tunnel.
func TestHysteriaLinkAsEntryOnAnotherServer(t *testing.T) {
	needRealXray(t)
	echo := startEcho(t)
	exitPort, exited := startSocks5(t)

	// ---- server A: plain xray running the generated server config ----------
	useFreeXrayPorts(t)
	dirA := t.TempDir()
	oldCert := xray.TLSCertFunc
	installTLSCertGen(paths.Paths{Cache: dirA})
	t.Cleanup(func() { xray.TLSCertFunc = oldCert })

	hy, err := xray.NewInboundFromTemplate("hy", "hysteria2", "xray:exit")
	if err != nil {
		t.Fatal(err)
	}
	hy.Listen, hy.Port = "127.0.0.1", freePort(t)
	// A second inbound on A, used by B as a relay in front of hysteria.
	relay, err := xray.NewInboundFromTemplate("relay", "vmess-tcp", "direct")
	if err != nil {
		t.Fatal(err)
	}
	relay.Listen, relay.Port = "127.0.0.1", freePort(t)
	cfg, err := xray.Generate(
		[]xray.XrayEntry{{Name: "exit", Enabled: true, Outbound: socksMember(exitPort)}},
		[]xray.Inbound{*hy, *relay}, nil, xray.GenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	binA, err := xraybin.Extract(dirA)
	if err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(dirA, "a.json")
	if err := writeFileAtomic(cfgPath, cfg); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(binA, "run", "-c", cfgPath)
	cmd.Env = append(os.Environ(), "XRAY_LOCATION_ASSET="+dirA)
	if os.Getenv("EMX_TEST_LOG") != "" {
		cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	waitListening(t, relay.Port) // same process; hysteria (UDP) is up with it

	// ---- server B: emx supervisor fed only A's share links ---------------
	useFreeXrayPorts(t) // B's api/slot/metrics must not collide with A's
	store, sup := newRealSupervisor(t)
	hyLink := xray.ShareLink(*hy, "127.0.0.1")
	relayLink := xray.ShareLink(*relay, "127.0.0.1")
	t.Logf("hysteria link: %s", hyLink)
	addLink := func(name, link, dialer string) {
		t.Helper()
		pl, err := xray.ParseLink(link)
		if err != nil {
			t.Fatalf("%s: parse link: %v", name, err)
		}
		e := &xray.XrayEntry{Name: name, Enabled: true, Outbound: string(pl.Outbound), Dialer: dialer}
		if err := store.CreateEntry(e); err != nil {
			t.Fatal(err)
		}
	}
	masterPort, _ := startSocks5(t) // a master reached through hysteria
	addLink("hy", hyLink, "")
	addLink("relay", relayLink, "")
	addLink("hy-behind-relay", hyLink, "xray:relay")
	if err := store.CreateEntry(&xray.XrayEntry{Name: "M", Enabled: true, Dialer: "xray:hy", Outbound: socksMember(masterPort)}); err != nil {
		t.Fatal(err)
	}

	// blocked: a hysteria master. xray's hysteria client ignores dialerProxy, so
	// emx refuses one on every write path; one stored anyway (older build) must
	// fail closed rather than dial A straight off B, skipping the relay.
	cases := []struct {
		name, target string
		blocked      bool
	}{
		{"inbound target is the hysteria entry", "xray:hy", false},
		{"hysteria entry is a master's pool member", "master:M", false},
		{"stored hysteria master fails closed", "master:hy-behind-relay", true},
	}
	gates := make([]int, len(cases))
	for i, c := range cases {
		gates[i] = freePort(t)
		in := &xray.Inbound{Name: "gate" + strconv.Itoa(i), Enabled: true, Protocol: "socks",
			Listen: "127.0.0.1", Port: gates[i], Target: c.target}
		if err := store.CreateInbound(in); err != nil {
			t.Fatal(err)
		}
	}
	if err := sup.Reconcile(); err != nil {
		t.Fatal(err)
	}
	for _, g := range gates {
		waitListening(t, g)
	}

	for i, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			before := exited()
			if c.blocked {
				d, err := xproxy.SOCKS5("tcp", "127.0.0.1:"+strconv.Itoa(gates[i]), nil, xproxy.Direct)
				if err != nil {
					t.Fatal(err)
				}
				if conn, err := d.Dial("tcp", echo); err == nil {
					if tryRoundTrip(conn, "leak?") == nil {
						t.Fatal("hysteria master carried traffic")
					}
					_ = conn.Close()
				}
				if exited() != before {
					t.Fatal("hysteria master reached server A")
				}
				return
			}
			conn := dialVia(t, gates[i], echo)
			roundTrip(t, conn, "via "+c.target)
			_ = conn.Close()
			if exited() == before {
				t.Fatal("traffic did not cross server A's hysteria inbound")
			}
		})
	}
}
