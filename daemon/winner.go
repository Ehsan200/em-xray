package daemon

import (
	"context"
	"sort"
	"strconv"
	"strings"

	"github.com/ehsan200/em-xray/core/xray"
)

// Winner is what one master's balancer currently routes to.
type Winner struct {
	Master  string
	Node    string   // human name of the winning member ("" if none yet)
	Nodes   []string // every member the balancer currently spreads over
	Tag     string   // slotN-out-<key> ("" if none)
	Members int      // resolved pool size; 0 means the balancer has nothing to pick
	Alive   int      // members whose latest pings succeed; -1 = unknown (no metrics yet)
}

// Winners queries the running xray for each master's current balancer winner and
// maps it back to a human node name. It reads the slots of the config xray is
// actually running, not a fresh resolve. Returns nil when xray isn't running
// or there are no masters.
func (s *Supervisor) Winners() ([]Winner, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.wd.IsStarted() {
		return nil, nil
	}
	slots := s.loadedSlots
	if len(slots) == 0 {
		return nil, nil
	}
	// One balancer query per slot; masters sharing a slot share its winner.
	balTags := make([]string, 0, len(slots))
	for _, sl := range slots {
		balTags = append(balTags, xray.SlotBalTag(sl.Index))
	}
	raw, err := s.BalancerInfoRaw(balTags...)
	if err != nil {
		return nil, err
	}
	selects := xray.ParseBalancerSelects(raw)
	health, herr := fetchObservatory(context.Background(), "127.0.0.1:"+strconv.Itoa(xray.MetricsPort))

	var out []Winner
	for _, sl := range slots {
		names := make(map[string]string, len(sl.Members))
		for _, m := range sl.Members {
			names[xray.SlotMemberTag(sl.Index, m.Key)] = s.memberName(m.Key)
		}
		// A ranked pool's balancer is `random` over the daemon's active pair,
		// and xray lists that whole selector — dead members too, which it
		// skips when routing. Show what it routes to: the live ones, or the
		// spare (its fallback) once none is live.
		ranked := len(sl.Picked) > 0
		dead := func(tag string) bool {
			st, ok := health[tag]
			return herr == nil && ok && st.dead()
		}
		var tags []string
		for _, tag := range selects[xray.SlotBalTag(sl.Index)] {
			if _, ok := names[tag]; !ok {
				continue // stale pick for a member already removed
			}
			if ranked && dead(tag) {
				continue
			}
			tags = append(tags, tag)
		}
		if ranked && len(tags) == 0 && len(sl.Picked) > xray.SlotBalancerExpected {
			if spare := xray.SlotMemberTag(sl.Index, sl.Picked[xray.SlotBalancerExpected]); names[spare] != "" {
				tags = []string{spare}
			}
		}
		var wtag, node string
		var nodes []string
		for _, tag := range tags {
			if wtag == "" {
				wtag, node = tag, names[tag]
			}
			nodes = append(nodes, names[tag])
		}
		alive := -1
		if herr == nil {
			alive = 0
			for tag := range names {
				if health[tag].Alive {
					alive++
				}
			}
		}
		for _, master := range sl.SlotMasters() {
			out = append(out, Winner{Master: master, Node: node, Nodes: nodes, Tag: wtag, Members: len(sl.Members), Alive: alive})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Master < out[j].Master })
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
