package daemon

import (
	"bytes"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/ehsan200/em-xray/internal/paths"
)

// reapGrace is how long a stray gets to exit on SIGTERM before SIGKILL.
const reapGrace = 3 * time.Second

// StrayXray lists xray processes started from emx's own extracted binary. A
// daemon killed with SIGKILL (or an OOM) leaves its child reparented to init,
// still holding the inbound ports — several of those accumulate over upgrades
// and, because the listeners share the port, silently serve an old config.
// Only emx's binary path matches, so an xray you run yourself is never touched.
func StrayXray(p paths.Paths, keep ...int) []int {
	keepSet := map[int]bool{os.Getpid(): true}
	for _, pid := range keep {
		if pid > 0 {
			keepSet[pid] = true
		}
	}
	var out []int
	for _, pid := range processPIDs() {
		if keepSet[pid] {
			continue
		}
		if !isEmxXray(pid, p.XrayBin()) {
			continue
		}
		out = append(out, pid)
	}
	return out
}

// ReapStrayXray terminates every stray and returns the pids it signalled.
func ReapStrayXray(p paths.Paths, logger *log.Logger, keep ...int) []int {
	strays := StrayXray(p, keep...)
	for _, pid := range strays {
		if logger != nil {
			logger.Printf("terminating orphaned xray (pid %d) still holding emx ports", pid)
		}
		_ = syscall.Kill(pid, syscall.SIGTERM)
	}
	if len(strays) == 0 {
		return nil
	}
	deadline := time.Now().Add(reapGrace)
	for time.Now().Before(deadline) {
		if !anyAlive(strays) {
			return strays
		}
		time.Sleep(100 * time.Millisecond)
	}
	for _, pid := range strays {
		if alive(pid) {
			if logger != nil {
				logger.Printf("orphaned xray (pid %d) ignored SIGTERM — killing", pid)
			}
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
	}
	return strays
}

func anyAlive(pids []int) bool {
	for _, pid := range pids {
		if alive(pid) {
			return true
		}
	}
	return false
}

// isEmxXray reports whether pid runs the xray binary emx extracted into its
// cache. Matching argv[0] keeps the check narrow: a system or hand-run xray has
// a different path and is left alone.
func isEmxXray(pid int, bin string) bool {
	argv0, ok := commandLine(pid)
	if !ok {
		return false
	}
	return argv0 == bin
}

func commandLine(pid int) (string, bool) {
	if b, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "cmdline")); err == nil {
		argv0, _, _ := bytes.Cut(b, []byte{0})
		return string(argv0), len(argv0) > 0
	}
	out, err := exec.Command("ps", "-p", strconv.Itoa(pid), "-o", "args=").Output()
	if err != nil {
		return "", false
	}
	fields := strings.Fields(string(out))
	if len(fields) == 0 {
		return "", false
	}
	return fields[0], true
}

// processPIDs enumerates live pids. /proc is authoritative on Linux; elsewhere
// (development on macOS) ps provides the same list.
func processPIDs() []int {
	if entries, err := os.ReadDir("/proc"); err == nil {
		var pids []int
		for _, e := range entries {
			if pid, err := strconv.Atoi(e.Name()); err == nil {
				pids = append(pids, pid)
			}
		}
		return pids
	}
	out, err := exec.Command("ps", "-axo", "pid=").Output()
	if err != nil {
		return nil
	}
	var pids []int
	for _, line := range strings.Fields(string(out)) {
		if pid, err := strconv.Atoi(line); err == nil {
			pids = append(pids, pid)
		}
	}
	return pids
}
