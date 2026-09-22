package paths

import (
	"os"
	"os/user"
	"path/filepath"
	"strconv"
)

// LegacyDataDirs lists data directories an older emx may have used for the
// current user, most likely first. Pinning root to /root and ignoring XDG for
// uid 0 fixed a split-brain database, but it also moves the lookup for anyone
// upgrading, so the daemon adopts a database found in one of these instead of
// starting empty.
func LegacyDataDirs() []string {
	return legacyDataDirs(os.Getuid(), os.Getenv("XDG_DATA_HOME"), envHome(), sudoHome(), Default().Data)
}

// LegacyRuntimeDirs lists runtime directories an older emx may have used, so a
// daemon still running under the previous layout can be detected instead of
// silently duplicated.
func LegacyRuntimeDirs() []string {
	uid := os.Getuid()
	return legacyRuntimeDirs(uid, os.Getenv("XDG_RUNTIME_DIR"), rootStableTemp(uid, os.TempDir()), Default().Runtime)
}

func legacyDataDirs(uid int, xdg, home, sudo, current string) []string {
	var candidates []string
	if xdg != "" {
		candidates = append(candidates, filepath.Join(xdg, app))
	}
	for _, h := range []string{home, sudo} {
		if h != "" {
			candidates = append(candidates, filepath.Join(h, ".local/share", app))
		}
	}
	if uid == 0 {
		candidates = append(candidates, filepath.Join("/root/.local/share", app))
	}
	return dedupeExcept(candidates, current)
}

func legacyRuntimeDirs(uid int, xdg, temp, current string) []string {
	var candidates []string
	if xdg != "" {
		candidates = append(candidates, filepath.Join(xdg, app))
	}
	if uid == 0 {
		candidates = append(candidates, filepath.Join("/run/user/0", app))
	}
	candidates = append(candidates, filepath.Join(temp, app+"-"+strconv.Itoa(uid)))
	return dedupeExcept(candidates, current)
}

func dedupeExcept(candidates []string, current string) []string {
	seen := map[string]bool{current: true}
	out := make([]string, 0, len(candidates))
	for _, c := range candidates {
		if c == "" || seen[c] {
			continue
		}
		seen[c] = true
		out = append(out, c)
	}
	return out
}

// envHome is the raw $HOME (or the passwd entry), i.e. what an older emx used
// before root was pinned to /root.
func envHome() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return home
}

// sudoHome resolves the home of the human behind `sudo emx`, whose database a
// root daemon should adopt rather than leave behind.
func sudoHome() string {
	name := os.Getenv("SUDO_USER")
	if name == "" || name == "root" {
		return ""
	}
	u, err := user.Lookup(name)
	if err != nil {
		return ""
	}
	return u.HomeDir
}

// RootRuntime is where a root daemon keeps its socket and pid file. A non-root
// CLI uses it to notice that the machine's emx state belongs to root, instead
// of quietly starting a second daemon with a second database.
func RootRuntime() string { return runtimeDirFor(0, "", rootStableTemp(0, os.TempDir())) }
