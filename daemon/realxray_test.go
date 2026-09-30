package daemon

import (
	"bufio"
	"encoding/binary"
	"io"
	"log"
	"math/rand"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ehsan200/em-xray/core/xray"
	"github.com/ehsan200/em-xray/internal/paths"
	"github.com/ehsan200/em-xray/internal/xraybin"
	xproxy "golang.org/x/net/proxy"
)

// Helpers for tests that run the real embedded xray. Everything binds free
// loopback ports: the live api/slot/metrics ports may belong to a running emx
// or em-wall, and nothing here may touch them (or any process it didn't start).

// needRealXray skips unless the embedded binary is present and -short is off.
func needRealXray(t *testing.T) {
	t.Helper()
	if testing.Short() {
		t.Skip("integration: runs a real xray")
	}
	if xraybin.Version() == "none" {
		t.Skip("no embedded xray: run scripts/fetch-xray.sh")
	}
}

// useFreeXrayPorts moves the api / slot / metrics ports onto free loopback
// ports for the duration of the test.
func useFreeXrayPorts(t *testing.T) {
	t.Helper()
	oldAPI, oldSlot, oldMetrics := xray.ApiPort, xray.SlotPortStart, xray.MetricsPort
	xray.ApiPort = freePort(t)
	xray.MetricsPort = freePort(t)
	xray.SlotPortStart = freePortRange(t, 4)
	t.Cleanup(func() { xray.ApiPort, xray.SlotPortStart, xray.MetricsPort = oldAPI, oldSlot, oldMetrics })
}

// newRealSupervisor builds a supervisor over a temp store/paths. Its watchdog
// runs the embedded xray against the temp config.
func newRealSupervisor(t *testing.T) (*xray.Store, *Supervisor) {
	t.Helper()
	dir := t.TempDir()
	p := paths.Paths{Data: dir, Config: dir, State: dir, Cache: dir, Runtime: dir}
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	store, err := xray.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	var logw io.Writer = io.Discard
	if os.Getenv("EMX_TEST_LOG") != "" {
		logw = os.Stderr
	}
	sup := NewSupervisor(store, p, log.New(logw, "", 0))
	t.Cleanup(func() { sup.Stop(); store.Close() })
	return store, sup
}

func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

// freePortRange finds n consecutive free loopback ports and returns the first.
// It searches below the OS ephemeral range, so later freePort() listeners
// can't land inside the reserved block.
func freePortRange(t *testing.T, n int) int {
	t.Helper()
	for try := 0; try < 200; try++ {
		base := 20000 + rand.Intn(20000)
		ok := true
		for i := 0; i < n && ok; i++ {
			ln, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(base+i))
			if err != nil {
				ok = false
				continue
			}
			ln.Close()
		}
		if ok {
			return base
		}
	}
	t.Fatal("no free port range")
	return 0
}

func startEcho(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() { defer c.Close(); _, _ = io.Copy(c, c) }()
		}
	}()
	return ln.Addr().String()
}

func startHTTP204(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return ln.Addr().String()
}

// startSocks5 runs a minimal no-auth SOCKS5 CONNECT server — a stand-in pool
// node (members must be real proxies, never freedom). accepted counts the
// connections it has served.
func startSocks5(t *testing.T) (port int, accepted func() int64) {
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
			go serveSocks5(c)
		}
	}()
	return ln.Addr().(*net.TCPAddr).Port, n.Load
}

func serveSocks5(c net.Conn) {
	defer c.Close()
	r := bufio.NewReader(c)
	hdr := make([]byte, 2)
	if _, err := io.ReadFull(r, hdr); err != nil || hdr[0] != 5 {
		return
	}
	if _, err := io.ReadFull(r, make([]byte, hdr[1])); err != nil {
		return
	}
	_, _ = c.Write([]byte{5, 0})
	req := make([]byte, 4)
	if _, err := io.ReadFull(r, req); err != nil || req[1] != 1 {
		return
	}
	var host string
	switch req[3] {
	case 1:
		b := make([]byte, 4)
		if _, err := io.ReadFull(r, b); err != nil {
			return
		}
		host = net.IP(b).String()
	case 3:
		l, err := r.ReadByte()
		if err != nil {
			return
		}
		b := make([]byte, l)
		if _, err := io.ReadFull(r, b); err != nil {
			return
		}
		host = string(b)
	case 4:
		b := make([]byte, 16)
		if _, err := io.ReadFull(r, b); err != nil {
			return
		}
		host = net.IP(b).String()
	default:
		return
	}
	pb := make([]byte, 2)
	if _, err := io.ReadFull(r, pb); err != nil {
		return
	}
	up, err := net.DialTimeout("tcp", net.JoinHostPort(host, strconv.Itoa(int(binary.BigEndian.Uint16(pb)))), 5*time.Second)
	if err != nil {
		_, _ = c.Write([]byte{5, 5, 0, 1, 0, 0, 0, 0, 0, 0})
		return
	}
	defer up.Close()
	_, _ = c.Write([]byte{5, 0, 0, 1, 0, 0, 0, 0, 0, 0})
	go func() { _, _ = io.Copy(up, r) }()
	_, _ = io.Copy(c, up)
}

// socksMember is a member outbound dialing a SOCKS5 server on a loopback port.
func socksMember(port int) string {
	return `{"protocol":"socks","settings":{"servers":[{"address":"127.0.0.1","port":` + strconv.Itoa(port) + `}]}}`
}

func waitListening(t *testing.T, port int) {
	t.Helper()
	a := "127.0.0.1:" + strconv.Itoa(port)
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if c, err := net.DialTimeout("tcp", a, 200*time.Millisecond); err == nil {
			_ = c.Close()
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("%s never started listening", a)
}

func dialVia(t *testing.T, socksPort int, target string) net.Conn {
	t.Helper()
	d, err := xproxy.SOCKS5("tcp", "127.0.0.1:"+strconv.Itoa(socksPort), nil, xproxy.Direct)
	if err != nil {
		t.Fatal(err)
	}
	c, err := d.Dial("tcp", target)
	if err != nil {
		t.Fatalf("dial via socks %d: %v", socksPort, err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// roundTrip writes a line and expects it echoed back.
func roundTrip(t *testing.T, c net.Conn, msg string) {
	t.Helper()
	if err := tryRoundTrip(c, msg); err != nil {
		t.Fatalf("%s: stream broken: %v", msg, err)
	}
}

func tryRoundTrip(c net.Conn, msg string) error {
	_ = c.SetDeadline(time.Now().Add(3 * time.Second))
	defer c.SetDeadline(time.Time{})
	if _, err := io.WriteString(c, msg+"\n"); err != nil {
		return err
	}
	line, err := bufio.NewReader(c).ReadString('\n')
	if err != nil {
		return err
	}
	if strings.TrimSpace(line) != msg {
		return io.ErrUnexpectedEOF
	}
	return nil
}
