// Package paths resolves all on-disk locations for emx following the XDG base
// directory spec, with sane fallbacks when the environment is unset.
//
// Layout:
//
//	data    $XDG_DATA_HOME/emx     (~/.local/share/emx)   sqlite db
//	config  $XDG_CONFIG_HOME/emx   (~/.config/emx)        user config
//	state   $XDG_STATE_HOME/emx    (~/.local/state/emx)   xray access/error logs
//	cache   $XDG_CACHE_HOME/emx    (~/.cache/emx)         extracted xray binary + geo assets
//	runtime $XDG_RUNTIME_DIR/emx   (/tmp/emx-<uid>)       socket, pid, generated config.json, ado temp
package paths

import (
	"os"
	"path/filepath"
	"runtime"
	"strconv"
)

const app = "emx"

// Paths holds every resolved directory and file location for one emx instance.
type Paths struct {
	Data    string
	Config  string
	State   string
	Cache   string
	Runtime string
}

// Default resolves paths from the current environment. It does not create any
// directories; call EnsureDirs for that.
func Default() Paths {
	return Paths{
		Data:    baseDir("XDG_DATA_HOME", ".local/share"),
		Config:  baseDir("XDG_CONFIG_HOME", ".config"),
		State:   baseDir("XDG_STATE_HOME", ".local/state"),
		Cache:   baseDir("XDG_CACHE_HOME", ".cache"),
		Runtime: runtimeDir(),
	}
}

// EnsureDirs creates every directory (0700 — these hold a control socket and db).
func (p Paths) EnsureDirs() error {
	for _, d := range []string{p.Data, p.Config, p.State, p.Cache, p.Runtime} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return err
		}
	}
	return nil
}

// Socket is the gRPC control socket the daemon binds and the CLI dials.
func (p Paths) Socket() string { return filepath.Join(p.Runtime, "emx.sock") }

// PIDFile records the running daemon's process id.
func (p Paths) PIDFile() string { return filepath.Join(p.Runtime, "emx.pid") }

// DB is the sqlite database (subs, nodes, entries, overrides).
func (p Paths) DB() string { return filepath.Join(p.Data, "emx.db") }

// XrayConfig is the generated xray config.json handed to the child process.
func (p Paths) XrayConfig() string { return filepath.Join(p.Runtime, "config.json") }

// XrayBin is the extracted xray executable.
func (p Paths) XrayBin() string { return filepath.Join(p.Cache, "xray") }

// AssetDir holds geoip.dat / geosite.dat (XRAY_LOCATION_ASSET points here).
func (p Paths) AssetDir() string { return p.Cache }

// AccessLog / ErrorLog are xray's log files (watched for 50MB rotation).
func (p Paths) AccessLog() string { return filepath.Join(p.State, "xray-access.log") }
func (p Paths) ErrorLog() string  { return filepath.Join(p.State, "xray-error.log") }

// AdoDir holds one-outbound JSON files fed to `xray api ado`.
func (p Paths) AdoDir() string { return filepath.Join(p.Runtime, "ado") }

func baseDir(env, fallback string) string {
	uid := os.Getuid()
	home, err := os.UserHomeDir()
	if err != nil {
		home = "/tmp"
	}
	// A Linux system service runs root with HOME=/root and no XDG variables,
	// while sudo policies may preserve the invoking user's HOME/XDG values.
	// Pin root to its conventional home so both processes open the same DB.
	if uid == 0 && runtime.GOOS == "linux" {
		home = "/root"
	}
	return baseDirFor(uid, os.Getenv(env), home, fallback)
}

func baseDirFor(uid int, xdg, home, fallback string) string {
	if uid != 0 && xdg != "" {
		return filepath.Join(xdg, app)
	}
	return filepath.Join(home, fallback, app)
}

// runtimeDir prefers $XDG_RUNTIME_DIR for normal users. Root deliberately uses
// the stable fallback: a system service normally has no XDG_RUNTIME_DIR while
// `sudo emx` may receive /run/user/0, which used to make the CLI and daemon use
// different sockets despite sharing the same database.
func runtimeDir() string {
	return runtimeDirFor(os.Getuid(), os.Getenv("XDG_RUNTIME_DIR"), rootStableTemp(os.Getuid(), os.TempDir()))
}

// rootStableTemp keeps root off $TMPDIR: a system service has none while an
// interactive `sudo emx` may inherit one, which would again split the CLI and
// the daemon across two sockets.
func rootStableTemp(uid int, temp string) string {
	if uid == 0 && runtime.GOOS == "linux" {
		return "/tmp"
	}
	return temp
}

func runtimeDirFor(uid int, xdg, temp string) string {
	if uid != 0 && xdg != "" {
		return filepath.Join(xdg, app)
	}
	return filepath.Join(temp, app+"-"+strconv.Itoa(uid))
}
