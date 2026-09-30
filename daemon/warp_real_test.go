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

// A WARP entry is a WireGuard outbound. Server A is a plain xray WireGuard
// server standing in for Cloudflare (WARP itself is unreachable from CI);
// server B is a full emx supervisor with the entry built exactly as
// `emx warp add` builds it, reserved bytes included, and a socks inbound
// routed to xray:warp. A connection only counts when it came out of A's
// tunnel, so this proves client → inbound → warp → net.
func TestWarpEntryCarriesTraffic(t *testing.T) {
	needRealXray(t)
	echo := startEcho(t)
	exitPort, exited := startSocks5(t)

	srvPriv, srvPub, err := xray.NewWireGuardKey()
	if err != nil {
		t.Fatal(err)
	}
	cliPriv, cliPub, err := xray.NewWireGuardKey()
	if err != nil {
		t.Fatal(err)
	}

	// ---- server A: xray WireGuard server egressing via the counting socks ----
	useFreeXrayPorts(t)
	dirA := t.TempDir()
	wgPort, readyPort := freePort(t), freePort(t)
	cfgA, _ := json.Marshal(map[string]any{
		"log": map[string]any{"loglevel": "warning"},
		"inbounds": []any{
			map[string]any{"tag": "wg", "listen": "127.0.0.1", "port": wgPort, "protocol": "wireguard",
				"settings": map[string]any{"secretKey": srvPriv, "mtu": 1280, "peers": []any{
					map[string]any{"publicKey": cliPub, "allowedIPs": []any{"172.16.0.2/32", "fd01::2/128"}},
				}}},
			// TCP port on the same process, only to know A is up.
			map[string]any{"tag": "ready", "listen": "127.0.0.1", "port": readyPort, "protocol": "socks",
				"settings": map[string]any{"auth": "noauth"}},
		},
		// WireGuard's netstack drops loopback destinations, so the client
		// dials a public-looking address and A redirects it to the echo —
		// through the counting socks.
		"outbounds": []any{
			map[string]any{"tag": "out", "protocol": "freedom", "settings": map[string]any{"redirect": echo},
				"streamSettings": map[string]any{"sockopt": map[string]any{"dialerProxy": "exit"}}},
			map[string]any{"tag": "exit", "protocol": "socks", "settings": map[string]any{
				"servers": []any{map[string]any{"address": "127.0.0.1", "port": exitPort}}}},
		},
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
	waitListening(t, readyPort)

	// ---- server B: emx with the WARP-shaped entry --------------------------
	useFreeXrayPorts(t)
	store, sup := newRealSupervisor(t)
	ob, err := xray.WireGuard{
		PrivateKey:    cliPriv,
		Addresses:     []string{"172.16.0.2", "fd01::2"},
		PeerPublicKey: srvPub,
		Endpoint:      "127.0.0.1:" + itoa(wgPort),
		Reserved:      []int{7, 8, 9},
		MTU:           1280,
	}.Outbound()
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CreateEntry(&xray.XrayEntry{Name: "warp", Enabled: true, Outbound: string(ob)}); err != nil {
		t.Fatal(err)
	}
	gate := freePort(t)
	if err := store.CreateInbound(&xray.Inbound{Name: "gate", Enabled: true, Protocol: "socks",
		Listen: "127.0.0.1", Port: gate, Target: "xray:warp"}); err != nil {
		t.Fatal(err)
	}
	if err := sup.Reconcile(); err != nil {
		t.Fatal(err)
	}
	waitListening(t, gate)

	before := exited()
	conn := dialVia(t, gate, "198.18.0.1:7")
	roundTrip(t, conn, "via warp")
	_ = conn.Close()
	if exited() == before {
		t.Fatal("traffic did not come out of the WireGuard server")
	}
}
