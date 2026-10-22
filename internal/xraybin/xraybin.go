// Package xraybin embeds the xray-core executable and geo data, extracting them
// to the cache dir on first use. A single asset set is embedded per build; which
// platform's binary is baked in is decided by scripts/fetch-xray.sh at build time
// (see the CI matrix — each OS job fetches its own triple).
package xraybin

import (
	"crypto/sha256"
	"embed"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

//go:embed assets/xray assets/geoip.dat assets/geosite.dat assets/VERSION
var assets embed.FS

// Version returns the embedded xray release tag (e.g. "v26.3.27"), or "none" if
// the placeholder assets were built (fetch-xray.sh never ran).
func Version() string {
	b, err := assets.ReadFile("assets/VERSION")
	if err != nil {
		return "unknown"
	}
	for _, line := range strings.Split(string(b), "\n") {
		if v, ok := strings.CutPrefix(line, "version="); ok {
			return strings.TrimSpace(v)
		}
	}
	return "unknown"
}

// Extract writes xray + geo assets into dir, but only if the on-disk copy is
// missing or its content differs from the embedded copy (hash-compared). The
// xray binary is made executable. Returns the path to the extracted binary.
func Extract(dir string) (string, error) {
	if Version() == "none" {
		return "", fmt.Errorf("xray assets not embedded: run scripts/fetch-xray.sh before building")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	files := map[string]os.FileMode{
		"xray":        0o755,
		"geoip.dat":   0o644,
		"geosite.dat": 0o644,
	}
	for name, mode := range files {
		if err := extractOne("assets/"+name, filepath.Join(dir, name), mode); err != nil {
			return "", fmt.Errorf("extract %s: %w", name, err)
		}
	}
	return filepath.Join(dir, "xray"), nil
}

// verified remembers, per extracted path, the stat stamp of the copy last
// checked against (or written from) the embed. Extract runs on every xray api
// call and probe round; re-reading and hashing ~60 MB each time kept that much
// garbage on the heap, which on a small box pushed the daemon into swap.
var verified sync.Map // dst -> stamp

type stamp struct {
	size    int64
	modTime time.Time
	mode    os.FileMode
}

func stampOf(fi os.FileInfo) stamp {
	return stamp{size: fi.Size(), modTime: fi.ModTime(), mode: fi.Mode().Perm()}
}

func extractOne(embedded, dst string, mode os.FileMode) error {
	if fi, err := os.Stat(dst); err == nil {
		if v, ok := verified.Load(dst); ok && v.(stamp) == stampOf(fi) && fi.Mode().Perm() == mode {
			return nil
		}
	}
	if same(dst, embedded) {
		if err := os.Chmod(dst, mode); err != nil { // still enforce mode
			return err
		}
		return remember(dst)
	}
	if err := writeEmbedded(embedded, dst+".tmp", mode); err != nil {
		return err
	}
	// atomic swap so a running xray isn't reading a half-written file
	if err := os.Rename(dst+".tmp", dst); err != nil {
		return err
	}
	return remember(dst)
}

func remember(dst string) error {
	fi, err := os.Stat(dst)
	if err != nil {
		return err
	}
	verified.Store(dst, stampOf(fi))
	return nil
}

// writeEmbedded streams the embedded file to path, without a heap copy.
func writeEmbedded(embedded, path string, mode os.FileMode) error {
	src, err := assets.Open(embedded)
	if err != nil {
		return err
	}
	defer src.Close()
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, src); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Chmod(path, mode) // OpenFile's mode is masked by umask
}

// same reports whether the file at path has exactly the embedded content.
func same(path, embedded string) bool {
	src, err := assets.Open(embedded)
	if err != nil {
		return false
	}
	defer src.Close()
	si, err := src.Stat()
	if err != nil {
		return false
	}
	fi, err := os.Stat(path)
	if err != nil || fi.Size() != si.Size() {
		return false
	}
	want, err := hashOf(src)
	if err != nil {
		return false
	}
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	got, err := hashOf(f)
	return err == nil && got == want
}

func hashOf(r io.Reader) ([sha256.Size]byte, error) {
	h := sha256.New()
	var sum [sha256.Size]byte
	if _, err := io.Copy(h, r); err != nil {
		return sum, err
	}
	copy(sum[:], h.Sum(nil))
	return sum, nil
}
