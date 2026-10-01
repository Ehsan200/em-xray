package daemon

import (
	"context"
	"net"
	"net/netip"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/ehsan200/em-xray/core/xray"
)

// Closing connections on a member traffic should leave: an agile pool's
// member that just died, or a manual pool's node that was unpinned.
//
// xray never moves an open connection: when the node carrying it stops
// answering, the stream just stalls, and the app behind it hangs until the
// idle timeout. In an agile pool nodes die every minute or so, so the daemon
// closes the dead member's connections to its server at once; the master's
// relay on top ends with them, the app reconnects, and the new connection
// rides a member that is alive. Only members that were carrying traffic (the
// pick) are cut, and never an endpoint a live member also uses — CDN-fronted
// nodes often share an address and port, and cutting it would take the live
// one's connections down too.

const memberCutResolveTimeout = 2 * time.Second

// cutDeadMembers closes the connections of every picked member of an agile
// pool that went from alive to dead in the latest poll.
func (s *Supervisor) cutDeadMembers(ctx context.Context, slots []xray.Slot, byTag map[string]nodeStatus) {
	if !sockCutSupported {
		return
	}
	var gone, keep []xray.SlotMember
	for _, sl := range slots {
		picked := map[string]bool{}
		for _, k := range sl.Picked {
			picked[k] = true
		}
		down := map[string]bool{}
		if sl.Agile() {
			for _, k := range s.picker.wentDown(sl) {
				if picked[k] {
					down[k] = true
				}
			}
		}
		for _, m := range sl.Members {
			switch {
			case down[m.Key]:
				gone = append(gone, m)
			case byTag[xray.SlotMemberTag(sl.Index, m.Key)].Alive:
				keep = append(keep, m)
			}
		}
	}
	s.cutMembers(ctx, gone, keep, "went down", "a live node")
}

// cutUnpinned closes, after a live apply, the connections of members a
// manual pool routed through before (old) but not now (cur): unpinned, or
// the pool just turned manual. Caller holds s.mu.
func (s *Supervisor) cutUnpinned(old, cur []xray.Slot) {
	if !sockCutSupported {
		return
	}
	prev := map[string]xray.Slot{}
	for _, sl := range old {
		prev[sl.Key] = sl
	}
	var gone, keep []xray.SlotMember
	for _, sl := range cur {
		was, ok := prev[sl.Key]
		if !sl.Manual() || !ok {
			continue
		}
		now := map[string]bool{}
		for _, k := range sl.Pinned {
			now[k] = true
		}
		active := map[string]bool{}
		for _, k := range was.Picked[:was.ActiveCount()] {
			active[k] = true
		}
		for _, m := range was.Members {
			if active[m.Key] && !now[m.Key] {
				gone = append(gone, m)
			}
		}
		for _, m := range sl.Members {
			if now[m.Key] {
				keep = append(keep, m)
			}
		}
	}
	s.cutMembers(context.Background(), gone, keep, "no longer pinned", "a pinned node")
}

// cutMembers closes every connection to the servers of gone, except an
// endpoint one of keep also uses. what says why for the log, to where the
// clients will reconnect.
func (s *Supervisor) cutMembers(ctx context.Context, gone, keep []xray.SlotMember, what, to string) {
	if len(gone) == 0 {
		return
	}
	type target struct {
		key, host string
		port      int
	}
	targets := func(ms []xray.SlotMember) []target {
		var out []target
		for _, m := range ms {
			if host, port, ok := xray.OutboundEndpoint(m.Outbound); ok {
				out = append(out, target{key: m.Key, host: host, port: port})
			}
		}
		return out
	}
	ctx, cancel := context.WithTimeout(ctx, memberCutResolveTimeout)
	defer cancel()
	resolve := func(t target) []netip.AddrPort {
		var addrs []netip.Addr
		if a, err := netip.ParseAddr(t.host); err == nil {
			addrs = []netip.Addr{a.Unmap()}
		} else if as, err := net.DefaultResolver.LookupNetIP(ctx, "ip4", t.host); err == nil {
			addrs = as
		}
		out := make([]netip.AddrPort, 0, len(addrs))
		for _, a := range addrs {
			out = append(out, netip.AddrPortFrom(a.Unmap(), uint16(t.port)))
		}
		return out
	}
	live := map[netip.AddrPort]bool{}
	for _, t := range targets(keep) {
		for _, ep := range resolve(t) {
			live[ep] = true
		}
	}
	var eps []netip.AddrPort
	var names, shared []string
	for _, t := range targets(gone) {
		var mine []netip.AddrPort
		skipped := false
		for _, ep := range resolve(t) {
			if live[ep] {
				skipped = true
				continue
			}
			mine = append(mine, ep)
		}
		name := s.memberName(t.key)
		if len(mine) == 0 {
			if skipped {
				shared = append(shared, name)
			}
			continue
		}
		eps = append(eps, mine...)
		names = append(names, name+" ("+net.JoinHostPort(t.host, strconv.Itoa(t.port))+")")
	}
	if len(shared) > 0 {
		sort.Strings(shared)
		s.log.Printf("%s %s, but shares its server address with %s — connections left open", strings.Join(shared, ", "), what, to)
	}
	if len(eps) == 0 {
		return
	}
	_, pid, _, _ := s.wd.State()
	n, err := cutRemoteEndpoints(pid, eps)
	if err != nil {
		s.log.Printf("%s %s, but its connections could not be closed (%v)", strings.Join(names, ", "), what, err)
		return
	}
	s.log.Printf("%s %s — closed %d connection(s) so clients reconnect through %s", strings.Join(names, ", "), what, n, to)
}
