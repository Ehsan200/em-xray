//go:build !linux

package daemon

import "os/exec"

// dieWithParent has no portable equivalent outside Linux; startup reaping
// covers orphans there.
func dieWithParent(*exec.Cmd) {}
