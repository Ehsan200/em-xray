package daemon

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/ehsan200/em-xray/core/xray"
	"github.com/ehsan200/em-xray/internal/xraybin"
)

// A WireGuard master (a WARP entry with a dialer) sends UDP through its
// dialer hop, so the slot inbound has to carry UDP and still know which
// master it came from to pick its balancer. The WireGuard endpoint is an
// address nothing answers on directly; only the pool's relay node redirects
// it to the WireGuard server, so a round trip proves the tunnel rode the pool.
func TestWireGuardMasterRidesPool(t *testing.T) {
	needRealXray(t)
	echo := startEcho(t)
	srvPriv, srvPub, err := xray.NewWireGuardKey()
	if err != nil {
		t.Fatal(err)
	}
	cliPriv, cliPub, err := xray.NewWireGuardKey()
	if err != nil {
		t.Fatal(err)
	}

	// ---- server A: WireGuard server + a vless relay node in front of it ----
	useFreeXrayPorts(t)
	dirA := t.TempDir()
	wgPort, relayPort := freePort(t), freePort(t)
	const relayID = "11111111-2222-4333-8444-555555555555"
	cfgA, _ := json.Marshal(map[string]any{
		"log": map[string]any{"loglevel": "warning"},
		"inbounds": []any{
			map[string]any{"tag": "wg", "listen": "127.0.0.1", "port": wgPort, "protocol": "wireguard",
				"settings": map[string]any{"secretKey": srvPriv, "mtu": 1280, "peers": []any{
					map[string]any{"publicKey": cliPub, "allowedIPs": []any{"172.16.0.2/32"}},
				}}},
			map[string]any{"tag": "relay", "listen": "127.0.0.1", "port": relayPort, "protocol": "vless",
				"settings": map[string]any{"decryption": "none", "clients": []any{map[string]any{"id": relayID}}}},
		},
		"outbounds": []any{
			map[string]any{"tag": "out", "protocol": "freedom", "settings": map[string]any{"redirect": echo}},
			map[string]any{"tag": "to-wg", "protocol": "freedom", "settings": map[string]any{"redirect": "127.0.0.1:" + itoa(wgPort)}},
		},
		"routing": map[string]any{"rules": []any{
			map[string]any{"type": "field", "inboundTag": []any{"relay"}, "outboundTag": "to-wg"},
			map[string]any{"type": "field", "inboundTag": []any{"wg"}, "outboundTag": "out"},
		}},
	})
	binA, err := xraybin.Extract(dirA)
	if err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(dirA, "a.json")
	if err := writeFileAtomic(cfgPath, cfgA); err != nil {
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
	waitListening(t, relayPort)

	// ---- server B: emx, WireGuard master behind a one-node pool -------------
	useFreeXrayPorts(t)
	store, sup := newRealSupervisor(t)
	ob, err := xray.WireGuard{
		PrivateKey:    cliPriv,
		Addresses:     []string{"172.16.0.2"},
		PeerPublicKey: srvPub,
		Endpoint:      "198.18.0.2:" + itoa(wgPort), // answers only via the relay
		MTU:           1280,
	}.Outbound()
	if err != nil {
		t.Fatal(err)
	}
	relay := `{"protocol":"vless","settings":{"vnext":[{"address":"127.0.0.1","port":` + itoa(relayPort) +
		`,"users":[{"id":"` + relayID + `","encryption":"none"}]}]}}`
	for _, e := range []xray.XrayEntry{
		{Name: "relay", Enabled: true, Outbound: relay},
		{Name: "other", Enabled: true, Dialer: "xray:relay", Outbound: socksMember(freePort(t))},
		{Name: "warp", Enabled: true, Dialer: "xray:relay", Outbound: string(ob)},
	} {
		if err := store.CreateEntry(&e); err != nil {
			t.Fatal(err)
		}
	}
	gate := freePort(t)
	if err := store.CreateInbound(&xray.Inbound{Name: "gate", Enabled: true, Protocol: "socks",
		Listen: "127.0.0.1", Port: gate, Target: "master:warp"}); err != nil {
		t.Fatal(err)
	}
	if err := sup.Reconcile(); err != nil {
		t.Fatal(err)
	}
	waitListening(t, gate)
	if sl := loadedSlot(sup); len(sl.SlotMasters()) != 2 {
		t.Fatalf("both masters must share the slot: %+v", sl)
	}

	conn := dialVia(t, gate, "198.18.0.1:7")
	roundTrip(t, conn, "wireguard via the pool")
	_ = conn.Close()
}
