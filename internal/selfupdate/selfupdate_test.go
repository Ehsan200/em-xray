package selfupdate

import "testing"

func TestNewer(t *testing.T) {
	cases := []struct {
		cur, lat string
		want     bool
	}{
		{"dev", "v1.0.0", true},        // dev build is always outdated
		{"", "v0.1.0", true},           // unknown current
		{"v1.0.0", "v1.0.1", true},     // patch bump
		{"v1.0.0", "v1.1.0", true},     // minor bump
		{"v1.9.0", "v2.0.0", true},     // major bump
		{"v1.0.1", "v1.0.0", false},    // remote older
		{"v1.2.3", "v1.2.3", false},    // equal
		{"v1.2.3", "v1.2.3-rc1", false},// prerelease of same version is not newer
		{"v1.0.0", "garbage", false},   // unparseable remote → no update
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
