package daemon

import (
	"sort"
	"sync"

	"github.com/ehsan200/em-xray/core/xray"
)

// Daemon-side member ranking. xray's leastLoad counts a node alive while any
// of its last 3 pings succeeded and ranks by RTT deviation only, so a flaky
// node with one lucky ping can beat a steady one. We score members over time
// and hand the balancer only the best nodePickCount; all stay loaded so the
// observatory keeps probing them.

const (
	nodePickCount    = 3
	nodeScoreAlpha   = 0.15 // EWMA weight per poll
	nodeHealthyRatio = 0.8
	nodeStickyRatio  = 1.25
	nodeStickyMs     = 40
)

const (
	tierHealthy = iota
	tierUnknown
	tierSuspect
	tierDead
)

type nodeScore struct {
	ok      float64 // EWMA success ratio
	rttMs   float64 // EWMA latency
	hasRTT  bool
	alive   bool // latest window has a success
	clean   bool // latest window has no failure
	sampled bool
}

// nodePicker keeps scores by member tag. Nil-safe.
type nodePicker struct {
	mu     sync.Mutex
	scores map[string]*nodeScore
}

func newNodePicker() *nodePicker { return &nodePicker{scores: map[string]*nodeScore{}} }

// observe folds one poll into the scores. A poll with nothing alive anywhere
// is the uplink, not the nodes, and is ignored (returns false).
func (p *nodePicker) observe(slots []xray.Slot, byTag map[string]nodeStatus) bool {
	if p == nil {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	loaded := map[string]bool{}
	anyAlive, anySampled := false, false
	for _, sl := range slots {
		for _, m := range sl.Members {
			tag := xray.SlotMemberTag(sl.Index, m.Key)
			loaded[tag] = true
			if st, ok := byTag[tag]; ok && st.HealthPing.All > 0 {
				anySampled = true
				anyAlive = anyAlive || st.Alive
			}
		}
	}
	for tag := range p.scores {
		if !loaded[tag] {
			delete(p.scores, tag)
		}
	}
	if anySampled && !anyAlive {
		return false
	}
	for tag := range loaded {
		st, ok := byTag[tag]
		sc := p.scores[tag]
		if !ok || st.HealthPing.All == 0 {
			if sc != nil {
				sc.sampled = false
			}
			continue
		}
		ratio := float64(st.HealthPing.All-st.HealthPing.Fail) / float64(st.HealthPing.All)
		if sc == nil {
			sc = &nodeScore{ok: ratio}
			p.scores[tag] = sc
		} else {
			sc.ok += nodeScoreAlpha * (ratio - sc.ok)
		}
		if st.Alive && st.Delay > 0 {
			if !sc.hasRTT {
				sc.rttMs, sc.hasRTT = float64(st.Delay), true
			} else {
				sc.rttMs += nodeScoreAlpha * (float64(st.Delay) - sc.rttMs)
			}
		}
		sc.alive, sc.clean, sc.sampled = st.Alive, st.HealthPing.Fail == 0, true
	}
	return true
}

func (sc *nodeScore) tier() int {
	switch {
	case sc == nil || !sc.sampled:
		return tierUnknown
	case !sc.alive:
		return tierDead
	case !sc.clean || sc.ok < nodeHealthyRatio || !sc.hasRTT:
		return tierSuspect
	}
	return tierHealthy
}

func (sc *nodeScore) cost() float64 {
	ok := sc.ok
	if ok < 0.05 {
		ok = 0.05
	}
	return sc.rttMs / (ok * ok)
}

// pick returns the keys to use: best first, rest sorted by key so order noise
// doesn't rewrite routing. Incumbents win unless clearly beaten. nil = no data.
func (p *nodePicker) pick(idx int, members []xray.SlotMember, incumbent []string) []string {
	if p == nil || len(members) == 0 {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	inc := map[string]bool{}
	for _, k := range incumbent {
		inc[k] = true
	}
	type cand struct {
		key  string
		tier int
		sc   *nodeScore
		raw  float64
		cost float64
	}
	cands := make([]cand, 0, len(members))
	scored := false
	for _, m := range members {
		sc := p.scores[xray.SlotMemberTag(idx, m.Key)]
		c := cand{key: m.Key, tier: sc.tier(), sc: sc}
		if c.tier != tierUnknown {
			scored = true
		}
		if c.tier == tierHealthy {
			c.raw = sc.cost()
			c.cost = c.raw
			if inc[m.Key] {
				c.cost = c.cost/nodeStickyRatio - nodeStickyMs
			}
		}
		cands = append(cands, c)
	}
	if !scored {
		return nil
	}
	sort.SliceStable(cands, func(i, j int) bool {
		a, b := cands[i], cands[j]
		if a.tier != b.tier {
			return a.tier < b.tier
		}
		switch a.tier {
		case tierHealthy:
			if a.cost != b.cost {
				return a.cost < b.cost
			}
		case tierSuspect, tierDead:
			if a.sc.ok != b.sc.ok {
				return a.sc.ok > b.sc.ok
			}
			if a.sc.hasRTT != b.sc.hasRTT {
				return a.sc.hasRTT
			}
			if a.sc.rttMs != b.sc.rttMs {
				return a.sc.rttMs < b.sc.rttMs
			}
		}
		if inc[a.key] != inc[b.key] {
			return inc[a.key]
		}
		return a.key < b.key
	})
	n := nodePickCount
	if n > len(cands) {
		n = len(cands)
	}
	out := make([]string, n)
	for i := range out {
		out[i] = cands[i].key
	}
	// Keep the previous best first unless the new best clearly beats it.
	if len(incumbent) > 0 {
		for i := range out {
			c, best := cands[i], cands[0]
			if c.key == incumbent[0] && c.tier == best.tier &&
				(c.tier != tierHealthy || c.raw/nodeStickyRatio-nodeStickyMs <= best.raw) {
				out[0], out[i] = out[i], out[0]
				break
			}
		}
	}
	sort.Strings(out[1:])
	return out
}

// pickSlots sets each slot's Picked, prev's picks of the same pool as incumbents.
func (p *nodePicker) pickSlots(slots, prev []xray.Slot) {
	was := map[string][]string{}
	for _, sl := range prev {
		was[sl.Key] = sl.Picked
	}
	for i := range slots {
		slots[i].Picked = p.pick(slots[i].Index, slots[i].Members, was[slots[i].Key])
	}
}

func samePick(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
