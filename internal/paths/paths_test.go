package paths

import "testing"

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
