//go:build linux

package daemon

import (
	"os/exec"
	"syscall"
)

// dieWithParent asks the kernel to signal the xray child when this daemon goes
// away, so a SIGKILLed or OOM-killed daemon cannot leave an orphan holding the
// inbound ports. Linux-only; other platforms fall back to reaping at startup.
func dieWithParent(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Pdeathsig = syscall.SIGTERM
}
