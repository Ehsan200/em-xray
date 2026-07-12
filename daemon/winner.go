package daemon

import (
	"strings"

	"github.com/gravisun/em-xray/core/xray"
)

// Winner is the balancer-selected (fastest) member for one master.
type Winner struct {
	Master string
	Node   string // human name of the winning member ("" if none yet)
	Tag    string // slotN-out-<key> ("" if none)
}

// Winners queries the running xray for each master's current balancer winner and
// maps it back to a human node name. Returns nil when xray isn't running or
// there are no masters.
func (s *Supervisor) Winners() ([]Winner, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.wd.IsStarted() {
		return nil, nil
	}
	entries, err := s.store.ListEntries()
	if err != nil {
		return nil, err
	}
	slots, err := s.resolveDialerSlots(entries)
	if err != nil {
		return nil, err
	}
	if len(slots) == 0 {
		return nil, nil
	}
	idx := xray.SlotIndexByName(slots)

	balTags := make([]string, 0, len(slots))
	for _, sl := range slots {
		balTags = append(balTags, xray.SlotBalTag(idx[sl.Master]))
	}
	raw, err := s.BalancerInfoRaw(balTags...)
	if err != nil {
		return nil, err
	}
	winners := xray.ParseBalancerWinners(raw)

	out := make([]Winner, 0, len(slots))
	for _, sl := range slots {
		i := idx[sl.Master]
		wtag := winners[xray.SlotBalTag(i)]
		w := Winner{Master: sl.Master, Tag: wtag}
		for _, m := range sl.Members {
			if xray.SlotMemberTag(i, m.Key) == wtag {
				w.Node = s.memberName(m.Key)
				break
			}
		}
		out = append(out, w)
	}
	return out, nil
}

// memberName maps a slot member Key back to a human name: an "xray-NAME" key is
// that entry; otherwise it's a node fingerprint resolved via the store.
func (s *Supervisor) memberName(key string) string {
	if name, ok := strings.CutPrefix(key, "xray-"); ok {
		return name
	}
	return s.store.NodeNameByFingerprint(key)
}
