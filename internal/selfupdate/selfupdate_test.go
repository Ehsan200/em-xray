package selfupdate

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestNewer(t *testing.T) {
	cases := []struct {
		cur, lat string
		want     bool
	}{
		{"dev", "v1.0.0", true},         // dev build is always outdated
		{"", "v0.1.0", true},            // unknown current
		{"v1.0.0", "v1.0.1", true},      // patch bump
		{"v1.0.0", "v1.1.0", true},      // minor bump
		{"v1.9.0", "v2.0.0", true},      // major bump
		{"v1.0.1", "v1.0.0", false},     // remote older
		{"v1.2.3", "v1.2.3", false},     // equal
		{"v1.2.3", "v1.2.3-rc1", false}, // prerelease of same version is not newer
		{"v1.0.0", "garbage", false},    // unparseable remote → no update
	}
	for _, c := range cases {
		if got := Newer(c.cur, c.lat); got != c.want {
			t.Errorf("Newer(%q,%q) = %v, want %v", c.cur, c.lat, got, c.want)
		}
	}
}

func TestAssetURL(t *testing.T) {
	rel := &Release{Assets: []Asset{
		{Name: "emx-v1.0.0-linux-amd64.tar.gz", URL: "u-lin"},
		{Name: "emx-v1.0.0-darwin-arm64.tar.gz", URL: "u-mac"},
	}}
	if u, ok := rel.AssetURL("linux", "amd64"); !ok || u != "u-lin" {
		t.Errorf("linux/amd64 = %q,%v", u, ok)
	}
	if u, ok := rel.AssetURL("darwin", "arm64"); !ok || u != "u-mac" {
		t.Errorf("darwin/arm64 = %q,%v", u, ok)
	}
	if _, ok := rel.AssetURL("windows", "amd64"); ok {
		t.Error("windows/amd64 should not match")
	}
}

// tarGz builds a .tar.gz holding a single `emx` entry with the given payload.
func tarGz(t *testing.T, payload []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	hdr := &tar.Header{Name: "emx", Mode: 0o755, Size: int64(len(payload)), Typeflag: tar.TypeReg}
	if err := tw.WriteHeader(hdr); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(payload); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestNewClientProxy(t *testing.T) {
	cases := []struct{ in, want string }{
		{"127.0.0.1:1080", "socks5h://127.0.0.1:1080"}, // bare host:port → socks5h
		{"socks5h://127.0.0.1:1080", "socks5h://127.0.0.1:1080"},
		{"http://10.0.0.1:8080", "http://10.0.0.1:8080"},
	}
	req, _ := http.NewRequest(http.MethodGet, "https://api.github.com/x", nil)
	for _, c := range cases {
		cl, err := NewClient(c.in, "", "")
		if err != nil {
			t.Fatalf("NewClient(%q): %v", c.in, err)
		}
		tr := cl.Transport.(*http.Transport)
		u, err := tr.Proxy(req)
		if err != nil || u == nil {
			t.Fatalf("NewClient(%q): proxy = %v, %v", c.in, u, err)
		}
		if u.String() != c.want {
			t.Errorf("NewClient(%q) proxies to %q, want %q", c.in, u, c.want)
		}
	}
	// A whole-exchange Timeout would re-introduce the bug this replaced: it
	// kills a slow-but-live download of a multi-megabyte tarball.
	cl, _ := NewClient("", "", "")
	if cl.Timeout != 0 {
		t.Errorf("client has a whole-request Timeout of %s; the body must be bounded by stall, not by a stopwatch", cl.Timeout)
	}
}

// TestDownloadOutlivesSlowLink is the regression for `emx update` failing with
// "context deadline exceeded" on a shaped connection. The server trickles the
// tarball out over well past the old 60s-for-everything budget's per-byte pace;
// as long as bytes keep coming, the download must finish.
func TestDownloadOutlivesSlowLink(t *testing.T) {
	payload := bytes.Repeat([]byte("x"), 64<<10)
	blob := tarGz(t, payload)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", strconv.Itoa(len(blob)))
		w.WriteHeader(http.StatusOK)
		flusher := w.(http.Flusher)
		// 20 dribbles, each well inside the stall window but slow in aggregate.
		chunk := (len(blob) / 20) + 1
		for off := 0; off < len(blob); off += chunk {
			end := min(off+chunk, len(blob))
			if _, err := w.Write(blob[off:end]); err != nil {
				return
			}
			flusher.Flush()
			time.Sleep(15 * time.Millisecond)
		}
	}))
	defer srv.Close()

	var lastDone, lastTotal int64
	got, err := download(context.Background(), srv.Client(), srv.URL,
		func(done, total int64) { lastDone, lastTotal = done, total }, 2*time.Second)
	if err != nil {
		t.Fatalf("slow-but-live download failed: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("payload mismatch: got %d bytes, want %d", len(got), len(payload))
	}
	if lastTotal != int64(len(blob)) {
		t.Errorf("progress total = %d, want %d", lastTotal, len(blob))
	}
	if lastDone == 0 {
		t.Error("progress callback never fired")
	}
}

// TestDownloadStallFailsFast pins the other half: a connection that opens, sends
// headers, then goes silent must fail with a legible message rather than hang.
func TestDownloadStallFailsFast(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		<-release // never sends a body byte
	}))
	defer srv.Close()
	defer close(release)

	start := time.Now()
	_, err := download(context.Background(), srv.Client(), srv.URL, nil, 300*time.Millisecond)
	if err == nil {
		t.Fatal("a silent connection must fail")
	}
	if !strings.Contains(err.Error(), "stalled") {
		t.Errorf("error should name the stall, got %v", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("stall took %s to surface, want ~300ms", elapsed)
	}
}

func TestNewClientProxyCredentials(t *testing.T) {
	req, _ := http.NewRequest(http.MethodGet, "https://api.github.com/x", nil)
	proxyURL := func(t *testing.T, c *http.Client) *url.URL {
		t.Helper()
		u, err := c.Transport.(*http.Transport).Proxy(req)
		if err != nil || u == nil {
			t.Fatalf("proxy = %v, %v", u, err)
		}
		return u
	}

	// Explicit user/pass on a bare host:port.
	cl, err := NewClient("127.0.0.1:1080", "alice", "s3cret")
	if err != nil {
		t.Fatal(err)
	}
	u := proxyURL(t, cl)
	if got, _ := u.User.Password(); u.User.Username() != "alice" || got != "s3cret" {
		t.Errorf("credentials not attached: %v", u.Redacted())
	}

	// A password full of URL metacharacters must survive verbatim — this is why
	// the separate arguments exist instead of only inline userinfo.
	nasty := "p@ss:w/rd?#&=x"
	cl, err = NewClient("socks5h://127.0.0.1:1080", "bob", nasty)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := proxyURL(t, cl).User.Password(); got != nasty {
		t.Errorf("password mangled: got %q, want %q", got, nasty)
	}

	// Inline userinfo works on its own, and explicit arguments override it.
	cl, err = NewClient("socks5h://inline:pw@127.0.0.1:1080", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if got := proxyURL(t, cl).User.Username(); got != "inline" {
		t.Errorf("inline user = %q, want inline", got)
	}
	cl, err = NewClient("socks5h://inline:pw@127.0.0.1:1080", "override", "np")
	if err != nil {
		t.Fatal(err)
	}
	u = proxyURL(t, cl)
	if got, _ := u.User.Password(); u.User.Username() != "override" || got != "np" {
		t.Errorf("explicit args must win over inline userinfo, got %v", u.Redacted())
	}

	// Credentials apply to an environment proxy too.
	t.Setenv("HTTPS_PROXY", "http://10.0.0.1:8080")
	cl, err = NewClient("", "carol", "pw")
	if err != nil {
		t.Fatal(err)
	}
	u = proxyURL(t, cl)
	if u.Host != "10.0.0.1:8080" || u.User.Username() != "carol" {
		t.Errorf("env proxy + credentials = %v", u.Redacted())
	}
}

func TestNewClientRejectsBadProxy(t *testing.T) {
	for _, bad := range []string{"vless://host:443", "socks4://host:1080", "socks5h://"} {
		if _, err := NewClient(bad, "", ""); err == nil {
			t.Errorf("NewClient(%q) should have failed", bad)
		}
	}
}

// TestProxyCredentialsReachAnHTTPProxy drives a real (http) proxy to prove the
// credentials make it onto the wire, not just onto the URL struct.
func TestProxyCredentialsReachAnHTTPProxy(t *testing.T) {
	var gotAuth string
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Proxy-Authorization")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(tarGz(t, []byte("payload")))
	}))
	defer proxy.Close()

	cl, err := NewClient(proxy.URL, "alice", "s3cret")
	if err != nil {
		t.Fatal(err)
	}
	// Plain http target so the proxy handles it as an absolute-URI request
	// rather than a CONNECT tunnel.
	if _, err := download(context.Background(), cl, "http://example.invalid/emx.tar.gz", nil, 5*time.Second); err != nil {
		t.Fatalf("download through authenticated proxy: %v", err)
	}
	want := "Basic " + base64.StdEncoding.EncodeToString([]byte("alice:s3cret"))
	if gotAuth != want {
		t.Errorf("Proxy-Authorization = %q, want %q", gotAuth, want)
	}
}
