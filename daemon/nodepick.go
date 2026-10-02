package daemon

import (
	"sort"
	"sync"

	"github.com/ehsan200/em-xray/core/xray"
)

// Daemon-side member ranking. xray's leastLoad counts a node alive while any
// ping in its window succeeded and ranks by RTT deviation (jitter) only, with
// the average merely breaking ties — a steady 900ms node beats a 90ms one that
// wobbles, and with a handful of samples the jitter is mostly noise. So we
// score members over time ourselves and hand xray the result: the best
// xray.SlotBalancerExpected become the balancer's selector (xray only drops
// the dead ones among them), the next one its fallback. All members stay
// loaded so the observatory keeps probing them.

const (
	nodePickCount    = 3
	agileActiveMax   = 4    // an agile pool spreads over at most this many live members
	nodeScoreAlpha   = 0.15 // EWMA weight per poll
	nodeHealthyRatio = 0.8
	nodeStickyRatio  = 1.25
	nodeStickyMs     = 40
)

const (
	tierHealthy = iota
	tierUnknown
	tierSuspect
	tierChainBad // answers pings, but fails the master's own chain probe
	tierDead
)

// Chain classes: what probing one master through a member found.
const (
	chainGood = iota
	chainUnknown
	chainBad
)

// chainView is one master's chain verdicts by member key. A nil view knows
// nothing: every member is chainUnknown, which ranks as before chain probing.
type chainView map[string]chainVerdict

type chainVerdict struct {
	class int
	cost  float64 // chain latency over its pass rate; set for chainGood
}

func (v chainView) of(key string) chainVerdict {
	if c, ok := v[key]; ok {
		return c
	}
	return chainVerdict{class: chainUnknown}
}

type nodeScore struct {
	ok      float64 // EWMA success ratio
	rttMs   float64 // EWMA latency
	lastRTT float64 // latest round's latency (agile ranks by it)
	hasRTT  bool
	alive   bool // latest window has a success
	clean   bool // latest window has no failure
	sampled bool
	// wentDown: alive in the previous poll, dead in this one.
	wentDown bool
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
	for _, sc := range p.scores {
		sc.wentDown = false
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
		sc.wentDown = sc.sampled && sc.alive && !st.Alive
		if st.Alive && st.Delay > 0 {
			sc.lastRTT = float64(st.Delay)
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

// pick returns the keys to use: the active members (xray.SlotBalancerExpected
// of them, sorted by key so rank noise between them doesn't rewrite routing),
// then the spare (the balancer's fallback). An incumbent keeps its place — in
// the pick, and again among the active — unless clearly beaten, so a pool
// doesn't flap between near-equal nodes. nil = no data.
//
// With a chain view (one master's probes through each member), a member that
// fails the master's chain ranks below every member that might carry it, and
// one proven to carry it ranks above the unproven, by its chain latency rather
// than the ping's: a node can answer the observatory and still not carry the
// master. Returns the keys and how many are active: the active never include
// a chain-failed member while one that isn't failed is picked.
func (p *nodePicker) pick(idx int, members []xray.SlotMember, incumbent []string, cv chainView) ([]string, int) {
	if p == nil || len(members) == 0 {
		return nil, 0
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.pickStableLocked(idx, members, incumbent, cv)
}

func (p *nodePicker) pickStableLocked(idx int, members []xray.SlotMember, incumbent []string, cv chainView) ([]string, int) {
	inc, wasActive := map[string]bool{}, map[string]bool{}
	for i, k := range incumbent {
		inc[k] = true
		if i < xray.SlotBalancerExpected {
			wasActive[k] = true
		}
	}
	type cand struct {
		key   string
		tier  int
		class int
		sc    *nodeScore
		raw   float64
		cost  float64
		inc   bool
	}
	cands := make([]cand, 0, len(members))
	scored := false
	for _, m := range members {
		sc := p.scores[xray.SlotMemberTag(idx, m.Key)]
		v := cv.of(m.Key)
		c := cand{key: m.Key, tier: sc.tier(), class: v.class, sc: sc, inc: inc[m.Key]}
		if c.tier != tierUnknown {
			scored = true
		}
		if c.class == chainBad && c.tier != tierDead {
			c.tier = tierChainBad
		}
		if c.tier == tierHealthy {
			c.raw = sc.cost()
			if c.class == chainGood {
				c.raw = v.cost
			}
			c.cost = c.raw
		}
		cands = append(cands, c)
	}
	if !scored {
		return nil, 0
	}
	rank := func(cs []cand) {
		for i := range cs {
			if cs[i].tier == tierHealthy {
				cs[i].cost = cs[i].raw
				if cs[i].inc {
					cs[i].cost = cs[i].cost/nodeStickyRatio - nodeStickyMs
				}
			}
		}
		sort.SliceStable(cs, func(i, j int) bool {
			a, b := cs[i], cs[j]
			if a.tier != b.tier {
				return a.tier < b.tier
			}
			if a.class != b.class {
				return a.class < b.class
			}
			switch a.tier {
			case tierHealthy:
				if a.cost != b.cost {
					return a.cost < b.cost
				}
			case tierSuspect, tierChainBad, tierDead:
				if (a.sc == nil) != (b.sc == nil) {
					return b.sc == nil
				}
				if a.sc == nil {
					break
				}
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
			if a.inc != b.inc {
				return a.inc
			}
			return a.key < b.key
		})
	}
	// Which members are picked at all: any previous pick is sticky.
	rank(cands)
	n := nodePickCount
	if n > len(cands) {
		n = len(cands)
	}
	top := cands[:n]
	// Which of those are active: only the previously active are sticky, so
	// the spare has to clearly beat an active member to take its place.
	for i := range top {
		top[i].inc = wasActive[top[i].key]
	}
	rank(top)
	active := xray.SlotBalancerExpected
	if active > n {
		active = n
	}
	// Never spread onto a chain-failed member while one that isn't failed
	// leads; when every one failed, fail open over the pool's own order.
	for active > 1 && top[0].tier != tierChainBad && top[active-1].tier == tierChainBad {
		active--
	}
	// A chain-failed member is never the spare either (the spare is the
	// balancer's fallback) unless every picked member failed.
	if top[0].tier != tierChainBad {
		for i := active; i < n; i++ {
			if top[i].tier == tierChainBad {
				n = i
				break
			}
		}
	}
	out := make([]string, n)
	for i := range out {
		out[i] = top[i].key
	}
	sort.Strings(out[:active])
	return out, active
}

// pickAgile is the agile pool's pick: every member alive in the latest round,
// fastest first by that round's RTT, up to agileActiveMax, then the spare —
// the next live member, else the one with the best recent record. No
// smoothing: a pool whose nodes come and go in waves has to follow the wave.
// Members already active keep their place unless clearly beaten, so RTT noise
// alone doesn't rewrite routing every poll. With nothing alive it falls back
// to the stable ranking. Returns the keys and how many of them are active.
//
// With a chain view, members that fail the master's chain are left out of the
// active set while any other member is alive, and members proven to carry it
// lead, by chain latency.
func (p *nodePicker) pickAgile(idx int, members []xray.SlotMember, incumbent []string, incActive int, cv chainView) ([]string, int) {
	if p == nil || len(members) == 0 {
		return nil, 0
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	wasActive := map[string]bool{}
	for i, k := range incumbent {
		if i < incActive {
			wasActive[k] = true
		}
	}
	type cand struct {
		key   string
		sc    *nodeScore
		class int
		cost  float64
	}
	var alive, aliveBad, rest []cand
	scored := false
	for _, m := range members {
		sc := p.scores[xray.SlotMemberTag(idx, m.Key)]
		if sc == nil || !sc.sampled {
			rest = append(rest, cand{key: m.Key, sc: sc})
			continue
		}
		scored = true
		if !sc.alive {
			rest = append(rest, cand{key: m.Key, sc: sc})
			continue
		}
		v := cv.of(m.Key)
		c := cand{key: m.Key, sc: sc, class: v.class, cost: sc.lastRTT}
		if c.cost == 0 {
			c.cost = sc.rttMs
		}
		if v.class == chainGood {
			c.cost = v.cost
		}
		if wasActive[m.Key] {
			c.cost = c.cost/nodeStickyRatio - nodeStickyMs
		}
		if v.class == chainBad {
			aliveBad = append(aliveBad, c)
			continue
		}
		alive = append(alive, c)
	}
	if !scored {
		return nil, 0
	}
	byCost := func(cs []cand) {
		sort.SliceStable(cs, func(i, j int) bool {
			if cs[i].class != cs[j].class {
				return cs[i].class < cs[j].class
			}
			if cs[i].cost != cs[j].cost {
				return cs[i].cost < cs[j].cost
			}
			return cs[i].key < cs[j].key
		})
	}
	byCost(aliveBad)
	if len(alive) == 0 {
		// Nothing alive that might carry the master: the chain-failed live
		// ones are still better than the dead.
		alive, aliveBad = aliveBad, nil
	}
	if len(alive) == 0 {
		return p.pickStableLocked(idx, members, incumbent, cv)
	}
	byCost(alive)
	n := min(len(alive), agileActiveMax)
	out := make([]string, 0, n+1)
	for _, c := range alive[:n] {
		out = append(out, c.key)
	}
	sort.Strings(out)
	switch {
	case len(alive) > n:
		out = append(out, alive[n].key)
	case len(rest) > 0:
		sort.SliceStable(rest, func(i, j int) bool {
			a, b := rest[i].sc, rest[j].sc
			if (a == nil) != (b == nil) {
				return b == nil
			}
			if a != nil && a.ok != b.ok {
				return a.ok > b.ok
			}
			return rest[i].key < rest[j].key
		})
		out = append(out, rest[0].key)
	}
	return out, n
}

// pickFor ranks one slot by its strategy for one master's chain view (nil =
// the pool's own ranking), prev being that ranking's last pick (its
// incumbents). Returns the keys and the active count.
func (p *nodePicker) pickFor(sl xray.Slot, prev xray.MasterPick, cv chainView) ([]string, int) {
	if sl.Manual() {
		// The pins are the pick, all active; with no spare the balancer's
		// fallback is the first pin, so traffic never leaves them.
		return append([]string(nil), sl.Pinned...), len(sl.Pinned)
	}
	if sl.Agile() {
		return p.pickAgile(sl.Index, sl.Members, prev.Picked, prev.ActiveCount(), cv)
	}
	return p.pick(sl.Index, sl.Members, prev.Picked, cv)
}

// pickSlot sets one slot's picks: the pool's own, then each master's from its
// chain view. A master with no chain verdicts on any member yet, or on a
// manual pool, follows the pool's pick. prev is the same pool's last state.
func (p *nodePicker) pickSlot(sl *xray.Slot, prev xray.Slot, chain *chainScores) {
	sl.Picked, sl.Active = p.pickFor(*sl, xray.MasterPick{Picked: prev.Picked, Active: prev.Active}, nil)
	sl.MasterPicks = nil
	if sl.Manual() {
		return
	}
	for _, master := range sl.SlotMasters() {
		cv := chain.view(master, sl.Members)
		if len(cv) == 0 {
			continue
		}
		keys, active := p.pickFor(*sl, prev.PickOf(master), cv)
		if sl.MasterPicks == nil {
			sl.MasterPicks = map[string]xray.MasterPick{}
		}
		sl.MasterPicks[master] = xray.MasterPick{Picked: keys, Active: active}
	}
}

// pickSlots sets each slot's picks, prev's picks of the same pool as
// incumbents.
func (p *nodePicker) pickSlots(slots, prev []xray.Slot, chain *chainScores) {
	was := map[string]xray.Slot{}
	for _, sl := range prev {
		was[sl.Key] = sl
	}
	for i := range slots {
		p.pickSlot(&slots[i], was[slots[i].Key], chain)
	}
}

// poolOrder lists slot idx's members the observatory doesn't see dead, best
// first by the pool's own ranking (tier, then cost or success rate).
func (p *nodePicker) poolOrder(idx int, members []xray.SlotMember) []string {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	type cand struct {
		key  string
		tier int
		sc   *nodeScore
	}
	var cs []cand
	for _, m := range members {
		sc := p.scores[xray.SlotMemberTag(idx, m.Key)]
		if t := sc.tier(); t != tierDead {
			cs = append(cs, cand{m.Key, t, sc})
		}
	}
	sort.SliceStable(cs, func(i, j int) bool {
		a, b := cs[i], cs[j]
		if a.tier != b.tier {
			return a.tier < b.tier
		}
		if a.tier == tierHealthy {
			return a.sc.cost() < b.sc.cost()
		}
		if a.sc != nil && b.sc != nil && a.sc.ok != b.sc.ok {
			return a.sc.ok > b.sc.ok
		}
		return a.key < b.key
	})
	out := make([]string, len(cs))
	for i, c := range cs {
		out[i] = c.key
	}
	return out
}

// knownDead reports whether member tag failed every ping of the latest poll.
func (p *nodePicker) knownDead(tag string) bool {
	if p == nil {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	sc := p.scores[tag]
	return sc != nil && sc.sampled && !sc.alive
}

// wentDown returns the member keys of slot sl that were alive at the previous
// poll and are dead now.
func (p *nodePicker) wentDown(sl xray.Slot) []string {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []string
	for _, m := range sl.Members {
		if sc := p.scores[xray.SlotMemberTag(sl.Index, m.Key)]; sc != nil && sc.wentDown {
			out = append(out, m.Key)
		}
	}
	return out
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
