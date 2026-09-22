package daemon

import (
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"

	"github.com/ehsan200/em-xray/internal/paths"
)

// dbSidecars are sqlite's write-ahead files; copying the database without them
// would drop transactions that have not been checkpointed yet.
var dbSidecars = []string{"", "-wal", "-shm"}

// AdoptLegacyDB moves an existing database into the current location when this
// layout has none. Root path pinning (and any XDG change) relocates the lookup,
// so an upgrade would otherwise open an empty database and look as though every
// inbound, entry and subscription had vanished. The source is left in place.
func AdoptLegacyDB(p paths.Paths, logger *log.Logger) error {
	return adoptLegacyDB(p, paths.LegacyDataDirs(), logger)
}

func adoptLegacyDB(p paths.Paths, candidates []string, logger *log.Logger) error {
	if fileExists(p.DB()) {
		return nil
	}
	for _, dir := range candidates {
		src := filepath.Join(dir, "emx.db")
		if !fileExists(src) {
			continue
		}
		if err := copyDB(src, p.DB()); err != nil {
			return fmt.Errorf("adopt database from %s: %w", src, err)
		}
		if logger != nil {
			logger.Printf("adopted database from %s (previous layout) → %s", src, p.DB())
		}
		return nil
	}
	return nil
}

// LegacyDaemon reports a daemon still running under a previous runtime layout.
// Starting a second daemon against the same xray would fight over ports and the
// generated config, so callers refuse instead.
func LegacyDaemon(p paths.Paths) (string, int, bool) {
	for _, dir := range paths.LegacyRuntimeDirs() {
		legacy := p
		legacy.Runtime = dir
		if pid, ok := RunningPID(legacy); ok {
			return dir, pid, true
		}
	}
	return "", 0, false
}

func copyDB(src, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return err
	}
	for _, suffix := range dbSidecars {
		if err := copyDBFile(src+suffix, dst+suffix); err != nil {
			return err
		}
	}
	return nil
}

func copyDBFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		if os.IsNotExist(err) {
			return nil // sidecars are optional
		}
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		os.Remove(dst)
		return err
	}
	return out.Close()
}

func fileExists(path string) bool {
	st, err := os.Stat(path)
	return err == nil && st.Mode().IsRegular() && st.Size() > 0
}
