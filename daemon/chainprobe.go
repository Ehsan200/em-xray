package daemon

import (
	"context"
	"net"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/ehsan200/em-xray/core/xray"
	"github.com/ehsan200/em-xray/internal/xraybin"
)

// Probing each master through each pool member.
//
// The observatory pings every member on its own, so it can only say a node
// answers. That is not the path a master's traffic takes: the master dials its
// own server through the node, usually over TLS, and some nodes that answer
// every ping can't carry that (seen on a real subscription: one node relayed
// plain HTTP in 0.3s and failed every master chained through it; another
// carried one master and not the others). Ranking on the ping alone steered
// every master onto the fastest node, which was exactly the broken one.
//
// Nodes also get choked by the network between this box and them: some
// filtering lets a connection to a node start and kills it after a few KB,
// so a tiny ping reply passes while a TLS handshake (certificates alone are
// several KB) or any real transfer dies.
//
// So the daemon also probes the real path: a throwaway xray (as `emx entry
// test` uses) chains each master's outbound through a member and downloads
// chainProbeBytes over HTTPS through the pair; a stall is a failure. The
// verdicts are per master and feed that master's own ranking (nodepick.go),
// so masters sharing a pool can ride different nodes. The live xray is never
// touched.
//
// Data costs: per master, only its current pick and the pool's best-ranked
// candidates are probed — walking down the pool's order until chainWindow
// members not known to fail it are covered — each every chainProbeEvery.
//
// Blame: when every pair of a master fails in a round, its own server is the
// likelier culprit, so no node is blamed. Each round with any pair of a master
// also probes that master through its best proven node, as the control.
//
// Verdicts go stale (chainVerdictTTL) and are cleared whenever this box's
// network changes (its addresses change, or the uplink comes back after
// every node looked dead): which nodes get choked depends on the network.

const (
	chainProbeTick     = time.Minute      // how often the prober looks for due pairs
	chainProbeEvery    = 10 * time.Minute // a pair is re-probed this long after its last probe
	chainVerdictTTL    = 30 * time.Minute // older verdicts count as unknown
	chainProbeStart    = 30 * time.Second
	chainProbeMax      = 120 // pairs per round, bounding one round's run time
	chainWindow        = 4   // per master, candidates covered beyond those known to fail it
	chainProbeAttempts = 1
	chainProbeTimeout  = 12 * time.Second
	chainProbeBytes    = 64 << 10
	chainScoreAlpha    = 0.5
	chainGoodRatio     = 0.5 // EWMA pass rate at or above which a pair carries
	// ChainProbeURL is downloaded through master+member: HTTPS from a real
	// site, chainProbeBytes of it. A master's traffic is TLS and more than a
	// few KB, and a small plain-HTTP reply is what choked nodes pass.
	ChainProbeURL = "https://speed.cloudflare.com/__down?bytes=65536"
)

type chainScore struct {
	ok      float64 // EWMA pass rate
	rttMs   float64 // EWMA latency of passing rounds
	hasRTT  bool
	at      time.Time // last probed
	lastErr string
}

func (c *chainScore) verdict() chainVerdict {
	if c.ok < chainGoodRatio || !c.hasRTT {
		return chainVerdict{class: chainBad}
	}
	return chainVerdict{class: chainGood, cost: c.rttMs / (c.ok * c.ok)}
}

// chainScores holds chain probe results by master name, then member key.
// Nil-safe; a nil store knows nothing.
type chainScores struct {
	mu  sync.Mutex
	m   map[string]map[string]*chainScore
	now func() time.Time
}

func newChainScores() *chainScores {
	return &chainScores{m: map[string]map[string]*chainScore{}, now: time.Now}
}

// reset forgets every verdict (the network changed).
func (c *chainScores) reset() {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.m = map[string]map[string]*chainScore{}
}

// record folds one probe of master through member key into its score.
func (c *chainScores) record(master, key string, passed, attempts, ms int, err error, at time.Time) {
	if c == nil || attempts <= 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	byKey := c.m[master]
	if byKey == nil {
		byKey = map[string]*chainScore{}
		c.m[master] = byKey
	}
	ratio := float64(passed) / float64(attempts)
	sc := byKey[key]
	if sc == nil {
		sc = &chainScore{ok: ratio}
		byKey[key] = sc
	} else {
		sc.ok += chainScoreAlpha * (ratio - sc.ok)
	}
	if passed > 0 {
		if !sc.hasRTT {
			sc.rttMs, sc.hasRTT = float64(ms), true
		} else {
			sc.rttMs += chainScoreAlpha * (float64(ms) - sc.rttMs)
		}
	}
	sc.at, sc.lastErr = at, ""
	if passed == 0 && err != nil {
		sc.lastErr = err.Error()
	}
}

// view is master's verdicts on members; empty when it has none yet.
func (c *chainScores) view(master string, members []xray.SlotMember) chainView {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	byKey := c.m[master]
	if len(byKey) == 0 {
		return nil
	}
	var out chainView
	now := c.now()
	for _, m := range members {
		if sc, ok := byKey[m.Key]; ok && now.Sub(sc.at) < chainVerdictTTL {
			if out == nil {
				out = chainView{}
			}
			out[m.Key] = sc.verdict()
		}
	}
	return out
}

// due reports whether master through key should be probed now.
func (c *chainScores) due(master, key string, now time.Time) bool {
	if c == nil {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	sc := c.m[master][key]
	return sc == nil || now.Sub(sc.at) >= chainProbeEvery
}

// lastProbed is when master through key was last probed (zero = never).
func (c *chainScores) lastProbed(master, key string) time.Time {
	if c == nil {
		return time.Time{}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if sc := c.m[master][key]; sc != nil {
		return sc.at
	}
	return time.Time{}
}

// keep drops every score whose master or member is no longer loaded.
func (c *chainScores) keep(slots []xray.Slot) {
	if c == nil {
		return
	}
	present := map[string]map[string]bool{}
	for _, sl := range slots {
		for _, master := range sl.SlotMasters() {
			keys := map[string]bool{}
			for _, m := range sl.Members {
				keys[m.Key] = true
			}
			present[master] = keys
		}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for master, byKey := range c.m {
		keys, ok := present[master]
		if !ok {
			delete(c.m, master)
			continue
		}
		for k := range byKey {
			if !keys[k] {
				delete(byKey, k)
			}
		}
	}
}

// renameMaster moves a master's scores to its new name.
func (c *chainScores) renameMaster(old, name string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if byKey, ok := c.m[old]; ok {
		delete(c.m, old)
		c.m[name] = byKey
	}
}

// renameMember moves every master's score of a member to its new key.
func (c *chainScores) renameMember(old, key string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, byKey := range c.m {
		if sc, ok := byKey[old]; ok {
			delete(byKey, old)
			byKey[key] = sc
		}
	}
}

// ChainStatus is one master-through-member verdict, for display.
type ChainStatus struct {
	Probed bool
	Good   bool
	OK     float64 // EWMA pass rate
	RTTMs  int
	Err    string
}

// status returns master's verdict on key.
func (c *chainScores) status(master, key string) ChainStatus {
	if c == nil {
		return ChainStatus{}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	sc := c.m[master][key]
	if sc == nil || c.now().Sub(sc.at) >= chainVerdictTTL {
		return ChainStatus{}
	}
	return ChainStatus{Probed: true, Good: sc.verdict().class == chainGood, OK: sc.ok, RTTMs: int(sc.rttMs), Err: sc.lastErr}
}

// chainPair is one master to probe through one member.
type chainPair struct {
	master  string
	member  xray.SlotMember
	last    time.Time
	control bool // probed only to tell a dead master from bad nodes
}

// chainBytes is how much each chain probe must download.
func (s *Supervisor) chainBytes() int64 {
	if s.chainMinBytes < 0 {
		return 0
	}
	if s.chainMinBytes > 0 {
		return s.chainMinBytes
	}
	return chainProbeBytes
}

// networkChanged reports (once) that this box's addresses differ from the
// last call's.
func (s *Supervisor) networkChanged() bool {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return false
	}
	var parts []string
	for _, a := range addrs {
		if ip, ok := a.(*net.IPNet); ok && !ip.IP.IsLoopback() && !ip.IP.IsLinkLocalUnicast() {
			parts = append(parts, ip.String())
		}
	}
	sort.Strings(parts)
	fp := strings.Join(parts, ",")
	s.netMu.Lock()
	defer s.netMu.Unlock()
	changed := s.netFP != "" && fp != s.netFP
	s.netFP = fp
	return changed
}

// resetChains forgets every chain verdict so the next round re-tests all.
func (s *Supervisor) resetChains(why string) {
	s.log.Printf("chain probe: %s — re-testing every node for every master", why)
	s.chain.reset()
}

// StartChainProbe probes master+member chains in the background.
func (s *Supervisor) StartChainProbe(ctx context.Context) {
	go func() {
		select {
		case <-ctx.Done():
			return
		case <-time.After(chainProbeStart):
		}
		tk := time.NewTicker(chainProbeTick)
		defer tk.Stop()
		for {
			s.ProbeChains(ctx)
			select {
			case <-ctx.Done():
				return
			case <-tk.C:
			}
		}
	}()
}

// duePairs lists the master+member pairs to chain-probe now, least recently
// probed first, at most chainProbeMax. Per master: its current pick, then the
// pool's order (members the observatory sees dead are skipped — the pool
// routes around them anyway) until chainWindow members not known to fail it
// are covered; of those, the ones due. A master with any pair due also gets
// its best proven member as the round's control (see blame in ProbeChains).
// Manual pools are skipped: the pins are the pick.
func (s *Supervisor) duePairs(slots []xray.Slot, now time.Time) []chainPair {
	var pairs []chainPair
	for _, sl := range slots {
		if sl.Manual() {
			continue
		}
		byKey := make(map[string]xray.SlotMember, len(sl.Members))
		for _, m := range sl.Members {
			byKey[m.Key] = m
		}
		order := s.picker.poolOrder(sl.Index, sl.Members)
		for _, master := range sl.SlotMasters() {
			cv := s.chain.view(master, sl.Members)
			seen := map[string]bool{}
			var cands []string
			for _, k := range sl.PickOf(master).Picked {
				if !seen[k] && !s.picker.knownDead(xray.SlotMemberTag(sl.Index, k)) {
					seen[k] = true
					cands = append(cands, k)
				}
			}
			covered := 0
			for _, k := range cands {
				if cv.of(k).class != chainBad {
					covered++
				}
			}
			for _, k := range order {
				if covered >= chainWindow {
					break
				}
				if seen[k] {
					continue
				}
				seen[k] = true
				cands = append(cands, k)
				if cv.of(k).class != chainBad {
					covered++
				}
			}
			var mine []chainPair
			for _, k := range cands {
				if s.chain.due(master, k, now) {
					mine = append(mine, chainPair{master: master, member: byKey[k], last: s.chain.lastProbed(master, k)})
				}
			}
			if len(mine) == 0 {
				continue
			}
			if ctl, ok := bestProven(cv); ok && !s.chain.due(master, ctl, now) {
				mine = append(mine, chainPair{master: master, member: byKey[ctl], last: s.chain.lastProbed(master, ctl), control: true})
			}
			pairs = append(pairs, mine...)
		}
	}
	sort.SliceStable(pairs, func(i, j int) bool { return pairs[i].last.Before(pairs[j].last) })
	if len(pairs) > chainProbeMax {
		pairs = pairs[:chainProbeMax]
	}
	return pairs
}

// bestProven is the member with the cheapest good verdict in cv.
func bestProven(cv chainView) (string, bool) {
	best, cost := "", 0.0
	for k, v := range cv {
		if v.class == chainGood && (best == "" || v.cost < cost || (v.cost == cost && k < best)) {
			best, cost = k, v.cost
		}
	}
	return best, best != ""
}

// ProbeChains runs one round of due chain probes and applies any pick it
// changes.
func (s *Supervisor) ProbeChains(ctx context.Context) {
	s.mu.Lock()
	running := s.wd.IsStarted() && s.running != nil
	slots := s.loadedSlots
	s.mu.Unlock()
	if !running || len(slots) == 0 {
		return
	}
	if s.networkChanged() {
		s.resetChains("this box's network changed")
	}
	s.chain.keep(slots)
	pairs := s.duePairs(slots, time.Now())
	if len(pairs) == 0 {
		return
	}
	entries, err := s.store.ListEntries()
	if err != nil {
		return
	}
	masters := map[string]string{}
	for _, e := range entries {
		if e.Enabled && e.IsMaster() {
			if ob, err := xray.MasterProbeOutbound(e); err == nil {
				masters[e.Name] = ob
			}
		}
	}
	items := make([]xray.ProbeItem, 0, len(pairs))
	probed := make([]chainPair, 0, len(pairs))
	for _, p := range pairs {
		ob, ok := masters[p.master]
		if !ok {
			continue
		}
		items = append(items, xray.ProbeItem{Name: p.master, Outbound: ob, Via: p.member.Outbound})
		probed = append(probed, p)
	}
	if len(items) == 0 {
		return
	}
	bin, err := xraybin.Extract(s.paths.Cache)
	if err != nil {
		s.log.Printf("chain probe: %v", err)
		return
	}
	url := s.chainURL
	if url == "" {
		url = ChainProbeURL
	}
	res := xray.ProbeOutbounds(ctx, items, xray.ProbeOptions{
		Bin: bin, AssetDir: s.paths.AssetDir(), WorkDir: s.paths.Runtime,
		URL: url, Timeout: chainProbeTimeout, Attempts: chainProbeAttempts,
		MinBytes: s.chainBytes(),
	})
	if ctx.Err() != nil {
		return
	}
	now := time.Now()
	// Blame: a master none of whose pairs passed this round is down itself
	// (or its server is), as far as this round can tell — no node is blamed.
	passed := map[string]bool{}
	for i, r := range res {
		if r.Passed > 0 {
			passed[probed[i].master] = true
		}
	}
	down := map[string]bool{}
	var flipped []string
	for i, r := range res {
		p := probed[i]
		if !passed[p.master] {
			if !down[p.master] {
				down[p.master] = true
				s.log.Printf("chain probe: %s failed through every node probed (%v) — its server looks down; no node blamed", p.master, r.Err)
			}
			continue
		}
		before := s.chain.status(p.master, p.member.Key)
		s.chain.record(p.master, p.member.Key, r.Passed, chainProbeAttempts, r.LatencyMs, r.Err, now)
		after := s.chain.status(p.master, p.member.Key)
		if !before.Probed || before.Good != after.Good {
			state := "carries it"
			if !after.Good {
				state = "does NOT carry it"
				if after.Err != "" {
					state += " (" + after.Err + ")"
				}
			}
			flipped = append(flipped, p.master+" via "+s.memberName(p.member.Key)+": "+state)
		}
	}
	for _, f := range flipped {
		s.log.Printf("chain probe: %s", f)
	}
	if s.repicked(s.loadedSlotsSnapshot()) {
		if err := s.Reconcile(); err != nil {
			s.log.Printf("apply chain probe: %v", err)
		}
	}
}

func (s *Supervisor) loadedSlotsSnapshot() []xray.Slot {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.loadedSlots
}

// ChainStatuses returns every master's chain verdicts by member key.
func (s *Supervisor) ChainStatuses() map[string]map[string]ChainStatus {
	out := map[string]map[string]ChainStatus{}
	for _, sl := range s.loadedSlotsSnapshot() {
		for _, master := range sl.SlotMasters() {
			byKey := map[string]ChainStatus{}
			for _, m := range sl.Members {
				if st := s.chain.status(master, m.Key); st.Probed {
					byKey[m.Key] = st
				}
			}
			out[master] = byKey
		}
	}
	return out
}

// pickNames renders a pick's keys as node names, for logs.
func (s *Supervisor) pickNames(keys []string) string {
	names := make([]string, len(keys))
	for i, k := range keys {
		names[i] = s.memberName(k)
	}
	return strings.Join(names, ", ")
}
