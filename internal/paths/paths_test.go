package paths

import (
	"runtime"
	"testing"
)

func TestRuntimeDirRootIsStableAcrossEnvironments(t *testing.T) {
	want := "/tmp/emx-0"
	for _, xdg := range []string{"", "/run/user/0", "/another/runtime"} {
		if got := runtimeDirFor(0, xdg, "/tmp"); got != want {
			t.Fatalf("runtimeDirFor(root, %q) = %q, want %q", xdg, got, want)
		}
	}
}

func TestRuntimeDirNormalUserFollowsXDG(t *testing.T) {
	if got := runtimeDirFor(1000, "/run/user/1000", "/tmp"); got != "/run/user/1000/emx" {
		t.Fatalf("XDG runtime = %q", got)
	}
	if got := runtimeDirFor(1000, "", "/tmp"); got != "/tmp/emx-1000" {
		t.Fatalf("fallback runtime = %q", got)
	}
}

func TestBaseDirRootIgnoresInheritedHomeAndXDG(t *testing.T) {
	if got := baseDirFor(0, "/home/alice/data", "/root", ".local/share"); got != "/root/.local/share/emx" {
		t.Fatalf("root data dir = %q", got)
	}
}

func TestBaseDirNormalUserFollowsXDG(t *testing.T) {
	if got := baseDirFor(1000, "/srv/alice/data", "/home/alice", ".local/share"); got != "/srv/alice/data/emx" {
		t.Fatalf("user XDG data dir = %q", got)
	}
	if got := baseDirFor(1000, "", "/home/alice", ".local/share"); got != "/home/alice/.local/share/emx" {
		t.Fatalf("user fallback data dir = %q", got)
	}
}

func TestLegacyDataDirsCoverPreviousRootLayouts(t *testing.T) {
	got := legacyDataDirs(0, "/home/alice/.local/share", "/home/alice", "/home/alice", "/root/.local/share/emx")
	want := []string{"/home/alice/.local/share/emx", "/home/alice/.local/share/emx"}
	if len(got) != 1 || got[0] != want[0] {
		t.Fatalf("root legacy data dirs = %v", got)
	}
}

func TestLegacyDataDirsExcludeCurrent(t *testing.T) {
	got := legacyDataDirs(1000, "", "/home/alice", "", "/home/alice/.local/share/emx")
	if len(got) != 0 {
		t.Fatalf("current dir must not be offered as legacy: %v", got)
	}
}

func TestLegacyRuntimeDirsIncludeOldRootSocketDir(t *testing.T) {
	got := legacyRuntimeDirs(0, "/run/user/0", "/tmp", "/tmp/emx-0")
	want := []string{"/run/user/0/emx", "/run/user/0/emx"}
	if len(got) != 1 || got[0] != want[0] {
		t.Fatalf("root legacy runtime dirs = %v", got)
	}
}

func TestRootTempIgnoresTMPDIR(t *testing.T) {
	got := rootStableTemp(0, "/var/folders/private-tmp")
	want := "/tmp"
	if runtime.GOOS != "linux" {
		want = "/var/folders/private-tmp"
	}
	if got != want {
		t.Fatalf("root temp = %q, want %q", got, want)
	}
	if got := rootStableTemp(1000, "/var/folders/private-tmp"); got != "/var/folders/private-tmp" {
		t.Fatalf("user temp = %q", got)
	}
}
