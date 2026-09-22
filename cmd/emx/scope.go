package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/ehsan200/em-xray/internal/paths"
	"github.com/spf13/cobra"
)

// userScopeEnv opts out of the shared-scope guard for a user who really wants a
// private daemon and database alongside the root one.
const userScopeEnv = "EMX_USER_SCOPE"

// ensureSharedScope keeps one machine on one database. Root and non-root resolve
// different paths, so without this a `sudo emx` configuration is invisible to a
// plain `emx` (and vice versa) — the same inbounds appear to have vanished.
func ensureSharedScope() error {
	_, err := os.Stat(paths.RootRuntime())
	if !userScopeBlocked(os.Geteuid(), os.Getenv(userScopeEnv), err == nil) {
		return nil
	}
	return fmt.Errorf("this machine's emx state belongs to root (%s); run `sudo emx ...` so both use the same database, "+
		"or set %s=1 to keep a separate per-user daemon", paths.RootRuntime(), userScopeEnv)
}

// userScopeBlocked reports whether a non-root invocation must defer to the root
// instance rather than open its own scope.
func userScopeBlocked(uid int, override string, rootStateExists bool) bool {
	if uid == 0 || !rootStateExists {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(override)) {
	case "1", "true", "yes", "on":
		return false
	}
	return true
}

// scopeExempt lists commands that say nothing about a daemon's state and must
// keep working in any scope: version output and the systemd unit helpers.
func scopeExempt(cmd *cobra.Command) bool {
	for c := cmd; c != nil; c = c.Parent() {
		switch c.Name() {
		case "version", "systemd", "help", "completion":
			return true
		}
	}
	return false
}
