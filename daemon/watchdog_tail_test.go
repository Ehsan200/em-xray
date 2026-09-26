package daemon

import (
	"io"
	"log"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// A child that dies on startup must leave its own reason in lastErr, not just
// "exit status N".
func TestWatchdogKeepsStartupErrorTail(t *testing.T) {
	wd := NewWatchdog(func() (*exec.Cmd, error) {
		cmd := exec.Command("sh", "-c", `echo "loading"; echo "Failed to start: listen tcp :443: bind: address already in use" >&2; exit 255`)
		out := &tailWriter{}
		cmd.Stdout, cmd.Stderr = out, out
		return cmd, nil
	}, log.New(io.Discard, "", 0))
	wd.Start()
	defer wd.Stop(time.Second)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, _, _, lastErr := wd.State(); lastErr != "" {
			if !strings.Contains(lastErr, "exit status 255") || !strings.Contains(lastErr, "address already in use") {
				t.Fatalf("lastErr = %q", lastErr)
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("no lastErr recorded")
}
