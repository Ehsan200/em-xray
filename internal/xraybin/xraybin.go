// Package xraybin embeds the xray-core executable and geo data, extracting them
// to the cache dir on first use. A single asset set is embedded per build; which
// platform's binary is baked in is decided by scripts/fetch-xray.sh at build time
// (see the CI matrix — each OS job fetches its own triple).
package xraybin

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
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
		"xray":         0o755,
		"geoip.dat":    0o644,
		"geosite.dat":  0o644,
	}
	for name, mode := range files {
		if err := extractOne("assets/"+name, filepath.Join(dir, name), mode); err != nil {
			return "", fmt.Errorf("extract %s: %w", name, err)
		}
	}
	return filepath.Join(dir, "xray"), nil
}

func extractOne(embedded, dst string, mode os.FileMode) error {
	want, err := assets.ReadFile(embedded)
	if err != nil {
		return err
	}
	if same(dst, want) {
		return os.Chmod(dst, mode) // still enforce mode
	}
	tmp := dst + ".tmp"
	if err := os.WriteFile(tmp, want, mode); err != nil {
		return err
	}
	return os.Rename(tmp, dst) // atomic swap so a running xray isn't reading a half-written file
}

// same reports whether the file at path has exactly the given content.
func same(path string, want []byte) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return false
	}
	wantH := sha256.Sum256(want)
	return hex.EncodeToString(h.Sum(nil)) == hex.EncodeToString(wantH[:])
}
