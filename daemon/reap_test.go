package daemon

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/ehsan200/em-xray/internal/paths"
)

func TestIsEmxXrayMatchesOnlyOurBinaryPath(t *testing.T) {
	self, ok := commandLine(os.Getpid())
	if !ok || self == "" {
		t.Skip("process command line unavailable on this platform")
	}
	if !isEmxXray(os.Getpid(), self) {
		t.Fatalf("own argv0 %q should match itself", self)
	}
	if isEmxXray(os.Getpid(), "/usr/local/bin/xray") {
		t.Fatal("a different xray path must never match — it is not ours to kill")
	}
}

func TestStrayXrayNeverReturnsSelf(t *testing.T) {
	self, ok := commandLine(os.Getpid())
	if !ok || self == "" {
		t.Skip("process command line unavailable on this platform")
	}
	// The caller must never appear in its own kill list, whatever the scan
	// turns up on the host running the tests.
	for _, pid := range StrayXray(paths.Paths{Cache: filepath.Dir(self)}) {
		if pid == os.Getpid() {
			t.Fatal("scan returned its own pid")
		}
	}
}
