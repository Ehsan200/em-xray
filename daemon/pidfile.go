package daemon

import (
	"os"
	"strconv"
	"strings"
	"syscall"

	"github.com/gravisun/em-xray/internal/paths"
)

// RunningPID returns the daemon pid recorded in the pid file and whether that
// process is currently alive. A stale pid file (process gone) reports alive=false.
func RunningPID(p paths.Paths) (int, bool) {
	pid, err := readPID(p.PIDFile())
	if err != nil {
		return 0, false
	}
	return pid, alive(pid)
}

func readPID(path string) (int, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(strings.TrimSpace(string(b)))
}

func writePID(path string, pid int) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(strconv.Itoa(pid)), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// alive reports whether pid names a live process. signal 0 probes existence:
// nil (or EPERM — exists but not ours) means alive; ESRCH means gone.
func alive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || err == syscall.EPERM
}
