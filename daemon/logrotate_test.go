package daemon

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRollLog(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "xray-error.log")
	if err := os.WriteFile(path, []byte(strings.Repeat("noise\n", 1000)), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := rollLog(path); err != nil {
		t.Fatal(err)
	}
	// Original truncated to zero.
	fi, err := os.Stat(path)
	if err != nil || fi.Size() != 0 {
		t.Errorf("original not truncated: size=%d err=%v", fi.Size(), err)
	}
	// Previous generation holds the old content.
	prev, err := os.ReadFile(path + ".prev")
	if err != nil || !strings.Contains(string(prev), "noise") {
		t.Errorf(".prev missing rolled content: %v", err)
	}
}
