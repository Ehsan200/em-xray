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
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// Repo is the GitHub owner/name releases are pulled from.
const Repo = "Ehsan200/em-xray"

const maxDownload = 128 << 20 // 128 MiB safety cap for a release asset

// Transport tunables. Deliberately NO http.Client.Timeout: that caps the whole
// exchange including the body, which turns a slow-but-healthy download of a
// ~30 MiB release tarball into a hard failure on a throttled link. Liveness is
// enforced by stallTimeout instead — bytes must keep arriving, but they may
// take as long as they take.
const (
	dialTimeout    = 15 * time.Second
	tlsTimeout     = 20 * time.Second
	headerTimeout  = 45 * time.Second
	stallTimeout   = 60 * time.Second // no bytes for this long → give up
	metaHTTPTimout = 20 * time.Second // whole-request cap for the small JSON call
)

// NewClient builds the HTTP client update traffic uses.
//
// proxy is an explicit proxy URL (`socks5h://host:port`, `http://host:port`);
// empty falls back to the HTTPS_PROXY/HTTP_PROXY environment. A bare
// `host:port` is read as socks5h, since that is what an emx socks inbound
// speaks.
//
// user/pass authenticate to that proxy. Credentials may also be written inline
// (`socks5h://user:pass@host:port`), but the separate arguments are the
// reliable form: they are escaped here, so a password containing `@`, `:` or
// `/` needs no hand-encoding. When given, they override any inline userinfo —
// including on a proxy that came from the environment.
func NewClient(proxy, user, pass string) (*http.Client, error) {
	tr := &http.Transport{
		TLSHandshakeTimeout:   tlsTimeout,
		ResponseHeaderTimeout: headerTimeout,
		ForceAttemptHTTP2:     true,
	}
	tr.DialContext = (&net.Dialer{Timeout: dialTimeout, KeepAlive: 30 * time.Second}).DialContext

	pick := http.ProxyFromEnvironment
	if proxy = strings.TrimSpace(proxy); proxy != "" {
		u, err := parseProxyURL(proxy)
		if err != nil {
			return nil, err
		}
		pick = http.ProxyURL(u)
	}
	if user != "" || pass != "" {
		pick = withCredentials(pick, user, pass)
	}
	tr.Proxy = pick
	return &http.Client{Transport: tr}, nil
}

// proxySchemes are the schemes net/http can actually dial a proxy over.
var proxySchemes = map[string]bool{"http": true, "https": true, "socks5": true, "socks5h": true}

// parseProxyURL turns the --proxy argument into a URL, defaulting a bare
// host:port to socks5h (an emx socks inbound). socks5h keeps DNS on the proxy
// side, which is the point of tunnelling the update in the first place.
func parseProxyURL(proxy string) (*url.URL, error) {
	if !strings.Contains(proxy, "://") {
		proxy = "socks5h://" + proxy
	}
	u, err := url.Parse(proxy)
	if err != nil {
		return nil, fmt.Errorf("bad proxy %q: %w", proxy, err)
	}
	if !proxySchemes[u.Scheme] {
		return nil, fmt.Errorf("unsupported proxy scheme %q (want http, https, socks5 or socks5h)", u.Scheme)
	}
	if u.Host == "" {
		return nil, fmt.Errorf("proxy %q has no host:port", proxy)
	}
	return u, nil
}

// withCredentials wraps a proxy picker so the chosen URL carries user/pass.
// net/http reads these for both proxy kinds: socks5 username/password auth, and
// a Proxy-Authorization header for http(s).
func withCredentials(next func(*http.Request) (*url.URL, error), user, pass string) func(*http.Request) (*url.URL, error) {
	return func(r *http.Request) (*url.URL, error) {
		u, err := next(r)
		if err != nil || u == nil {
			return u, err
		}
		withUser := *u
		withUser.User = url.UserPassword(user, pass)
		return &withUser, nil
	}
}

// client returns c, or a default one when the caller passed nil.
func client(c *http.Client) *http.Client {
	if c != nil {
		return c
	}
	d, err := NewClient("", "", "")
	if err != nil {
		return http.DefaultClient
	}
	return d
}

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

// Latest fetches the latest published release for the repo. This is a small
// JSON call, so it carries its own short cap regardless of the caller's ctx —
// checking for an update must never be what makes a command hang.
func Latest(ctx context.Context, c *http.Client) (*Release, error) {
	ctx, cancel := context.WithTimeout(ctx, metaHTTPTimout)
	defer cancel()
	endpoint := "https://api.github.com/repos/" + Repo + "/releases/latest"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := client(c).Do(req)
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
// any long-running process (the daemon) afterwards. progress, if non-nil, is
// called as bytes arrive; total is -1 when the server sends no Content-Length.
func Apply(ctx context.Context, c *http.Client, url string, progress func(done, total int64)) error {
	bin, err := downloadBinary(ctx, c, url, progress)
	if err != nil {
		return err
	}
	return replaceSelf(bin)
}

// downloadBinary fetches a .tar.gz and returns the bytes of the `emx` entry.
//
// The release tarball is tens of megabytes, and the boxes emx runs on are
// exactly the ones with slow or shaped links. So the transfer is bounded by
// PROGRESS, not by a stopwatch: a separate timer cancels the request only when
// no bytes have arrived for stallTimeout. A download creeping along at 50 KiB/s
// finishes; a connection that dies mid-body fails within a minute instead of
// hanging until the caller's deadline.
func downloadBinary(ctx context.Context, c *http.Client, url string, progress func(done, total int64)) ([]byte, error) {
	return download(ctx, c, url, progress, stallTimeout)
}

// download is downloadBinary with the stall window injected, so tests can use a
// window shorter than a minute.
func download(ctx context.Context, c *http.Client, url string, progress func(done, total int64), stallAfter time.Duration) ([]byte, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	var stalled atomic.Bool
	stall := time.AfterFunc(stallAfter, func() {
		stalled.Store(true)
		cancel()
	})
	defer stall.Stop()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client(c).Do(req)
	if err != nil {
		return nil, downloadErr(err, &stalled)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("download: %s", resp.Status)
	}

	body := &progressReader{
		r:      io.LimitReader(resp.Body, maxDownload),
		total:  resp.ContentLength,
		tick:   func() { stall.Reset(stallAfter) },
		report: progress,
	}
	gz, err := gzip.NewReader(body)
	if err != nil {
		return nil, downloadErr(err, &stalled)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return nil, fmt.Errorf("archive has no emx binary")
		}
		if err != nil {
			return nil, downloadErr(err, &stalled)
		}
		if h.Typeflag == tar.TypeReg && filepath.Base(h.Name) == "emx" {
			b, err := io.ReadAll(io.LimitReader(tr, maxDownload))
			if err != nil {
				return nil, downloadErr(err, &stalled)
			}
			return b, nil
		}
	}
}

// downloadErr replaces the bare "context canceled" a stall cancellation
// produces with something that says what actually went wrong.
func downloadErr(err error, stalled *atomic.Bool) error {
	if stalled.Load() {
		return fmt.Errorf("download stalled: no data for %s (try `emx update --proxy 127.0.0.1:PORT` through one of your inbounds)", stallTimeout)
	}
	return err
}

// progressReader counts bytes on the way past, pokes the stall timer so a live
// transfer is never cancelled, and reports progress at most once a second.
type progressReader struct {
	r       io.Reader
	total   int64 // -1 when unknown
	done    int64
	tick    func()
	report  func(done, total int64)
	lastRep time.Time
}

func (p *progressReader) Read(b []byte) (int, error) {
	n, err := p.r.Read(b)
	if n > 0 {
		p.done += int64(n)
		p.tick()
		if p.report != nil && time.Since(p.lastRep) >= time.Second {
			p.lastRep = time.Now()
			p.report(p.done, p.total)
		}
	}
	return n, err
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
