package daemon

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/ehsan200/em-xray/internal/paths"
)

func TestAdoptLegacyDBCopiesDatabaseAndSidecars(t *testing.T) {
	root := t.TempDir()
	legacy := filepath.Join(root, "legacy")
	current := filepath.Join(root, "current")
	if err := os.MkdirAll(legacy, 0o700); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(legacy, "emx.db"), "database")
	write(t, filepath.Join(legacy, "emx.db-wal"), "journal")

	p := paths.Paths{Data: current}
	if err := adoptLegacyDB(p, []string{legacy}, nil); err != nil {
		t.Fatal(err)
	}
	if got := read(t, p.DB()); got != "database" {
		t.Fatalf("adopted db = %q", got)
	}
	if got := read(t, p.DB()+"-wal"); got != "journal" {
		t.Fatalf("adopted wal = %q", got)
	}
	if _, err := os.Stat(filepath.Join(legacy, "emx.db")); err != nil {
		t.Fatalf("source must be left in place: %v", err)
	}
}

func TestAdoptLegacyDBKeepsExistingDatabase(t *testing.T) {
	root := t.TempDir()
	legacy := filepath.Join(root, "legacy")
	current := filepath.Join(root, "current")
	for _, d := range []string{legacy, current} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	write(t, filepath.Join(legacy, "emx.db"), "old")
	write(t, filepath.Join(current, "emx.db"), "live")

	p := paths.Paths{Data: current}
	if err := adoptLegacyDB(p, []string{legacy}, nil); err != nil {
		t.Fatal(err)
	}
	if got := read(t, p.DB()); got != "live" {
		t.Fatalf("existing db was overwritten: %q", got)
	}
}

func TestAdoptLegacyDBIgnoresEmptyAndMissing(t *testing.T) {
	root := t.TempDir()
	empty := filepath.Join(root, "empty")
	if err := os.MkdirAll(empty, 0o700); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(empty, "emx.db"), "")

	p := paths.Paths{Data: filepath.Join(root, "current")}
	if err := adoptLegacyDB(p, []string{filepath.Join(root, "gone"), empty}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(p.DB()); !os.IsNotExist(err) {
		t.Fatalf("nothing should have been adopted: %v", err)
	}
}

func write(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
