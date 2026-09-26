package daemon

import (
	"io"
	"log"
	"net"
	"os"
	"path/filepath"
	"testing"
)

// A socket file left by a dead listener is removed; one still accepting is kept.
func TestClearStaleSockets(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "emxsock") // short path: sun_path is ~104 bytes
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	stale := filepath.Join(dir, "stale.sock")
	live := filepath.Join(dir, "live.sock")

	l, err := net.Listen("unix", stale)
	if err != nil {
		t.Fatal(err)
	}
	l.(*net.UnixListener).SetUnlinkOnClose(false)
	_ = l.Close() // file stays, nobody listens: what a killed xray leaves
	ll, err := net.Listen("unix", live)
	if err != nil {
		t.Fatal(err)
	}
	defer ll.Close()

	cfg := filepath.Join(dir, "config.json")
	body := `{"inbounds":[{"listen":"` + stale + `,0666"},{"listen":"` + live + `"},{"listen":"0.0.0.0"}]}`
	if err := os.WriteFile(cfg, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	reaped := false
	clearStaleSockets(cfg, log.New(io.Discard, "", 0), func() []int { reaped = true; return nil })

	if _, err := os.Lstat(stale); !os.IsNotExist(err) {
		t.Fatalf("stale socket still there (err=%v)", err)
	}
	if !isSocket(live) {
		t.Fatal("live socket was removed")
	}
	if !reaped {
		t.Fatal("a held socket did not trigger the leftover-xray reap")
	}
	if _, err := net.Listen("unix", stale); err != nil {
		t.Fatalf("path still not bindable: %v", err)
	}
}

// Freeing a held socket: once the holder is gone, the path is cleared for xray.
func TestClearStaleSocketsAfterReap(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "emxsock")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, "held.sock")
	l, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	l.(*net.UnixListener).SetUnlinkOnClose(false)
	cfg := filepath.Join(dir, "config.json")
	if err := os.WriteFile(cfg, []byte(`{"inbounds":[{"listen":"`+path+`,0666"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	// The "leftover xray" dies without unlinking, like a SIGKILLed one.
	clearStaleSockets(cfg, log.New(io.Discard, "", 0), func() []int { _ = l.Close(); return []int{1} })
	if _, err := net.Listen("unix", path); err != nil {
		t.Fatalf("socket not freed after reap: %v", err)
	}
}
