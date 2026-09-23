package daemon

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/ehsan200/em-xray/core/xray"
	"github.com/ehsan200/em-xray/internal/xraybin"
)

// Validating before applying.
//
// xray refuses a config WHOLE when any one object in it is invalid, and that
// refusal used to surface only as a child that wouldn't start: one pool node
// with a field this xray version rejects (a provider link with allowInsecure,
// say) took every inbound on the server down, and the next refresh re-broke it.
// So every generated config is checked with `xray run -test` first:
//
//   - a rejected POOL MEMBER is left out of its pool (remembered with xray's
//     reason until its content changes) and the config regenerated — the rest
//     of the pool, and everything else, keeps working;
//   - anything else (an entry, an inbound) is refused: the running xray keeps
//     its current config, and the error names the object and xray's reason.

var badObjectRe = regexp.MustCompile(`(?:inbound|outbound) config with tag (\S+)`)

// rejection is a pool member xray refused, keyed by member key.
type rejection struct {
	outbound string // the member content that was refused
	reason   string
}

// testConfig runs `xray run -test` on cfg. nil when valid, or when no xray
// binary is embedded (dev builds without assets can't validate).
func (s *Supervisor) testConfig(cfg []byte) error {
	if xraybin.Version() == "none" {
		return nil
	}
	bin, err := xraybin.Extract(s.paths.Cache)
	if err != nil {
		return nil
	}
	f, err := os.CreateTemp(s.paths.Runtime, "validate-*.json")
	if err != nil {
		return nil
	}
	path := f.Name()
	defer os.Remove(path)
	if _, err := f.Write(cfg); err != nil {
		_ = f.Close()
		return nil
	}
	_ = f.Close()
	cmd := exec.Command(bin, "run", "-test", "-c", path)
	cmd.Env = append(os.Environ(), "XRAY_LOCATION_ASSET="+s.paths.AssetDir())
	done := make(chan struct{})
	var out []byte
	var runErr error
	go func() { out, runErr = cmd.CombinedOutput(); close(done) }()
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		_ = cmd.Process.Kill()
		<-done
		return nil // can't tell; let the apply path find out
	}
	if runErr == nil {
		return nil
	}
	return fmt.Errorf("%s", xrayReason(string(out), filepath.Base(path)))
}

// xrayReason trims xray's "Failed to start: main: failed to load config
// files: [path] > " preamble down to the part that says what is wrong.
func xrayReason(out, file string) string {
	lines := strings.Split(strings.TrimSpace(out), "\n")
	last := strings.TrimSpace(lines[len(lines)-1])
	if i := strings.Index(last, file+"] > "); i >= 0 {
		last = last[i+len(file)+4:]
	}
	return last
}

// rejectedTag returns the tag of the object xray refused, if its error names one.
func rejectedTag(err error) string {
	if m := badObjectRe.FindStringSubmatch(err.Error()); m != nil {
		return m[1]
	}
	return ""
}

// withoutRejected drops members xray refused, as long as their content is
// what was refused (a changed node gets another chance).
func (s *Supervisor) withoutRejected(members []xray.SlotMember) []xray.SlotMember {
	s.rejectedMu.Lock()
	defer s.rejectedMu.Unlock()
	if len(s.rejected) == 0 {
		return members
	}
	out := members[:0:0]
	for _, m := range members {
		if r, ok := s.rejected[m.Key]; ok && r.outbound == m.Outbound {
			continue
		}
		out = append(out, m)
	}
	return out
}

// RejectedMembers reports pool members xray refuses, key → xray's reason.
func (s *Supervisor) RejectedMembers() map[string]string {
	s.rejectedMu.Lock()
	defer s.rejectedMu.Unlock()
	out := make(map[string]string, len(s.rejected))
	for k, r := range s.rejected {
		out[k] = r.reason
	}
	return out
}

func (s *Supervisor) reject(key string, r rejection) {
	s.rejectedMu.Lock()
	s.rejected[key] = r
	s.rejectedMu.Unlock()
}

// forgetRejectedExcept drops rejections for members no pool resolves any more.
func (s *Supervisor) forgetRejectedExcept(present map[string]bool) {
	s.rejectedMu.Lock()
	defer s.rejectedMu.Unlock()
	for k := range s.rejected {
		if !present[k] {
			delete(s.rejected, k)
		}
	}
}

// ConfigError is the last reason a generated config was refused ("" = the
// last reconcile applied cleanly).
func (s *Supervisor) ConfigError() string {
	s.cfgErrMu.Lock()
	defer s.cfgErrMu.Unlock()
	return s.cfgErr
}

func (s *Supervisor) setConfigError(msg string) {
	s.cfgErrMu.Lock()
	s.cfgErr = msg
	s.cfgErrMu.Unlock()
}
