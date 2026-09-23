package daemon

import (
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"testing"

	"github.com/ehsan200/em-xray/core/xray"
	"github.com/ehsan200/em-xray/internal/xraybin"
)

// Mux must actually collapse connections into shared tunnels: five streams
// through a mux entry reach the remote server over one TCP connection, while
// the same entry without mux opens five. The remote is a second real xray
// (VMess over WebSocket) behind a counting relay; toggling mux applies live.
func TestMuxSharesTunnels(t *testing.T) {
	needRealXray(t)
	useFreeXrayPorts(t)
	echo := startEcho(t)
	const uuid = "b831381d-6324-4d53-ad4f-8cda48b30811"

	// Remote server: vmess+ws → freedom.
	dir := t.TempDir()
	bin, err := xraybin.Extract(dir)
	if err != nil {
		t.Fatal(err)
	}
	srvPort := freePort(t)
	srvCfg := `{"inbounds":[{"tag":"in","listen":"127.0.0.1","port":` + strconv.Itoa(srvPort) + `,"protocol":"vmess",` +
		`"settings":{"clients":[{"id":"` + uuid + `"}]},"streamSettings":{"network":"ws","wsSettings":{"path":"/w"}}}],` +
		`"outbounds":[{"protocol":"freedom"}]}`
	cfgPath := filepath.Join(dir, "server.json")
	if err := os.WriteFile(cfgPath, []byte(srvCfg), 0o644); err != nil {
		t.Fatal(err)
	}
	srv := exec.Command(bin, "run", "-c", cfgPath)
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = srv.Process.Kill(); _ = srv.Wait() })
	waitListening(t, srvPort)

	relayPort, accepted := startCountingRelay(t, "127.0.0.1:"+strconv.Itoa(srvPort))
	outbound := `{"protocol":"vmess","settings":{"vnext":[{"address":"127.0.0.1","port":` + strconv.Itoa(relayPort) +
		`,"users":[{"id":"` + uuid + `","security":"auto"}]}]},"streamSettings":{"network":"ws","wsSettings":{"path":"/w"}}}`

	store, sup := newRealSupervisor(t)
	e := &xray.XrayEntry{Name: "e", Enabled: true, Outbound: outbound}
	if err := store.CreateEntry(e); err != nil {
		t.Fatal(err)
	}
	gate := freePort(t)
	if err := store.CreateInbound(&xray.Inbound{Name: "gate", Enabled: true, Protocol: "socks", Listen: "127.0.0.1", Port: gate, Target: "xray:e"}); err != nil {
		t.Fatal(err)
	}
	if err := sup.Reconcile(); err != nil {
		t.Fatal(err)
	}
	waitListening(t, gate)

	run := func() int64 {
		t.Helper()
		start := accepted()
		var conns []net.Conn
		for i := 0; i < 5; i++ {
			c := dialVia(t, gate, echo)
			roundTrip(t, c, "hello "+strconv.Itoa(i))
			conns = append(conns, c)
		}
		for _, c := range conns {
			_ = c.Close()
		}
		return accepted() - start
	}

	plain := run()
	e.Mux = true
	if err := store.UpdateEntry(e); err != nil {
		t.Fatal(err)
	}
	if err := sup.Reconcile(); err != nil {
		t.Fatal(err)
	}
	if restarts, _ := sup.Counters(); restarts != 0 {
		t.Fatalf("toggling mux restarted xray")
	}
	muxed := run()
	t.Logf("server TCP connections for 5 streams: plain %d, mux %d", plain, muxed)
	if plain < 5 {
		t.Fatalf("without mux: %d server connections for 5 streams, want 5", plain)
	}
	if muxed != 1 {
		t.Fatalf("with mux: %d server connections for 5 streams, want 1", muxed)
	}
}

// startCountingRelay forwards TCP to target and counts accepted connections.
func startCountingRelay(t *testing.T, target string) (int, func() int64) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	var n atomic.Int64
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			n.Add(1)
			go func() {
				defer c.Close()
				up, err := net.Dial("tcp", target)
				if err != nil {
					return
				}
				defer up.Close()
				go func() { _, _ = io.Copy(up, c) }()
				_, _ = io.Copy(c, up)
			}()
		}
	}()
	return ln.Addr().(*net.TCPAddr).Port, n.Load
}
