package xraybin

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestExtractRepeatIsCheapAndRepairs(t *testing.T) {
	if Version() == "none" {
		t.Skip("xray assets not embedded")
	}
	dir := t.TempDir()
	bin, err := Extract(dir)
	if err != nil {
		t.Fatal(err)
	}
	want, _ := assets.ReadFile("assets/xray")
	if got, _ := os.ReadFile(bin); !bytes.Equal(got, want) {
		t.Fatal("extracted xray differs from the embed")
	}

	// A verified copy is not re-read: the repeat must not allocate the assets.
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	for range 5 {
		if _, err := Extract(dir); err != nil {
			t.Fatal(err)
		}
	}
	runtime.ReadMemStats(&after)
	if n := after.TotalAlloc - before.TotalAlloc; n > 1<<20 {
		t.Fatalf("5 repeat extracts allocated %d bytes", n)
	}

	// A damaged copy is detected (its stamp changed) and rewritten.
	geo := filepath.Join(dir, "geoip.dat")
	if err := os.WriteFile(geo, []byte("broken"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Extract(dir); err != nil {
		t.Fatal(err)
	}
	wantGeo, _ := assets.ReadFile("assets/geoip.dat")
	if got, _ := os.ReadFile(geo); !bytes.Equal(got, wantGeo) {
		t.Fatal("damaged geoip.dat was not repaired")
	}

	// A wrong mode is restored.
	if err := os.Chmod(bin, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Extract(dir); err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Stat(bin); fi.Mode().Perm() != 0o755 {
		t.Fatalf("xray mode %v, want 0755", fi.Mode().Perm())
	}
}
