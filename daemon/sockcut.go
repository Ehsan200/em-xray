package daemon

import (
	"net/netip"
	"sort"
	"strings"

	"github.com/ehsan200/em-xray/core/xray"
)

// Closing a master's old connections when its dialer changes.
//
// A live apply points a master at its new pool without a restart, but xray
// never moves or ends a connection that is already open: it keeps riding the
// old path, and if that path is dead the app behind it (a phone's messenger,
// say) hangs until the idle timeout. An open stream can't be moved — the
// master's server holds its connection to the site — so the fix is to end it
// at once: the app sees the close, reconnects within a second, and the new
// connection takes the new dialer. The phone's VPN stays up.
//
// Each master's dialer hop leaves from its own loopback address
// (xray.DialerSourceAddr), so its connections are told apart from every other
// master's, including masters sharing its pool, and only they are closed.

// movedMasters returns the masters whose dialer changed between two slot sets:
// in a different pool now, or no longer in one. A master new in cur has
// nothing open to move.
func movedMasters(old, cur []xray.Slot) []string {
	keyOf := func(slots []xray.Slot) map[string]string {
		m := map[string]string{}
		for _, s := range slots {
			for _, name := range s.SlotMasters() {
				m[name] = s.Key
			}
		}
		return m
	}
	was, now := keyOf(old), keyOf(cur)
	var moved []string
	for name, key := range was {
		if k, ok := now[name]; !ok || k != key {
			moved = append(moved, name)
		}
	}
	sort.Strings(moved)
	return moved
}

// cutMovedMasters closes the open connections of every master whose dialer
// changed from old to cur. Called after a successful live apply, so the apps'
// reconnects land on the new path. Caller holds s.mu.
func (s *Supervisor) cutMovedMasters(old, cur []xray.Slot) {
	moved := movedMasters(old, cur)
	if len(moved) == 0 || !sockCutSupported {
		return
	}
	addrs := make([]netip.Addr, 0, len(moved))
	for _, name := range moved {
		if a, err := netip.ParseAddr(xray.DialerSourceAddr(name)); err == nil {
			addrs = append(addrs, a)
		}
	}
	_, pid, _, _ := s.wd.State()
	n, err := cutLoopbackSockets(pid, addrs)
	if err != nil {
		s.log.Printf("dialer changed for %s, but its old connections could not be closed (%v) — they end on their own at the idle timeout",
			strings.Join(moved, ", "), err)
		return
	}
	s.log.Printf("dialer changed for %s: closed %d old connection(s) so clients reconnect through the new dialer",
		strings.Join(moved, ", "), n)
}
