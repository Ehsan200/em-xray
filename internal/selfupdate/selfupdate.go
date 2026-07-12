// Package selfupdate checks GitHub releases for a newer emx and replaces the
// running binary in place.
package selfupdate

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Repo is the GitHub owner/name releases are pulled from.
const Repo = "Ehsan200/em-xray"

const maxDownload = 128 << 20 // 128 MiB safety cap for a release asset

// Release is the subset of a GitHub release we use.
type Release struct {
	Tag    string
	Assets []Asset
}

// Asset is one downloadable file on a release.
type Asset struct {
	Name string
	URL  string
}

// Latest fetches the latest published release for the repo.
func Latest(ctx context.Context) (*Release, error) {
	url := "https://api.github.com/repos/" + Repo + "/releases/latest"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("github releases: %s", resp.Status)
	}
	var raw struct {
		TagName string `json:"tag_name"`
		Assets  []struct {
			Name string `json:"name"`
			URL  string `json:"browser_download_url"`
		} `json:"assets"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&raw); err != nil {
		return nil, err
	}
	rel := &Release{Tag: raw.TagName}
	for _, a := range raw.Assets {
		rel.Assets = append(rel.Assets, Asset{Name: a.Name, URL: a.URL})
	}
	return rel, nil
}

// AssetURL returns the download URL of the tarball matching goos/goarch.
func (r *Release) AssetURL(goos, goarch string) (string, bool) {
	suffix := fmt.Sprintf("-%s-%s.tar.gz", goos, goarch)
	for _, a := range r.Assets {
		if strings.HasSuffix(a.Name, suffix) {
			return a.URL, true
		}
	}
	return "", false
}

// Newer reports whether latest is a newer version than current. A non-release
// current ("dev"/empty) is always considered outdated. Tags are vMAJOR.MINOR.PATCH.
func Newer(current, latest string) bool {
	cur, okc := parseVer(current)
	lat, okl := parseVer(latest)
	if !okl {
		return false // can't understand the remote tag → don't offer an update
	}
	if !okc {
		return true // dev build → any real release is "newer"
	}
	for i := 0; i < 3; i++ {
		if lat[i] != cur[i] {
			return lat[i] > cur[i]
		}
	}
	return false
}

func parseVer(v string) ([3]int, bool) {
	v = strings.TrimPrefix(strings.TrimSpace(v), "v")
	if i := strings.IndexAny(v, "-+"); i >= 0 {
		v = v[:i] // drop prerelease/build metadata
	}
	parts := strings.Split(v, ".")
	if len(parts) != 3 {
		return [3]int{}, false
	}
	var out [3]int
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil {
			return [3]int{}, false
		}
		out[i] = n
	}
	return out, true
}

// Apply downloads the tarball at url, extracts the emx binary, and atomically
// replaces the currently-running executable with it. The caller should restart
// any long-running process (the daemon) afterwards.
func Apply(ctx context.Context, url string) error {
	bin, err := downloadBinary(ctx, url)
	if err != nil {
		return err
	}
	return replaceSelf(bin)
}

// downloadBinary fetches a .tar.gz and returns the bytes of the `emx` entry.
func downloadBinary(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("download: %s", resp.Status)
	}
	gz, err := gzip.NewReader(io.LimitReader(resp.Body, maxDownload))
	if err != nil {
		return nil, err
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return nil, fmt.Errorf("archive has no emx binary")
		}
		if err != nil {
			return nil, err
		}
		if h.Typeflag == tar.TypeReg && filepath.Base(h.Name) == "emx" {
			return io.ReadAll(io.LimitReader(tr, maxDownload))
		}
	}
}

// replaceSelf writes data over the running executable via a same-dir temp file
// + atomic rename (unix allows replacing the file of a running process).
func replaceSelf(data []byte) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	dir := filepath.Dir(exe)
	tmp, err := os.CreateTemp(dir, ".emx-update-*")
	if err != nil {
		return fmt.Errorf("cannot write to %s (need write permission): %w", dir, err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op after a successful rename
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpName, 0o755); err != nil {
		return err
	}
	return os.Rename(tmpName, exe)
}
