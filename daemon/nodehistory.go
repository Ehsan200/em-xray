package daemon

import (
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/ehsan200/em-xray/core/xray"
)

// Pool health history.
//
// Every health poll is recorded as one round per loaded pool: each member's
// state (alive, dead, unknown, parked), its latest RTT and its role in the
// daemon's pick (active, spare, idle). A round where nothing anywhere is alive
// is marked uplink-down — that is this box's link, not the nodes — and the
// stats leave it out. The history only observes; nothing routes by it. It is
// what `emx health` draws and what tells a stable pool from a flapping one
// (nodes that come and go in waves), so the stats live here, in one place.
//
// xray refreshes health once per observatory round (interval × sampling), so
// with a poll every nodeHealthPollInterval consecutive rounds often repeat a
// state; the effective resolution is the observatory round. In memory only:
// a daemon restart starts it over.

const nodeHistoryWindow = 30 * time.Minute

// healthState is a member's state in one round. Values are the wire encoding
// of PoolHealthNode.states.
type healthState byte

const (
	hsNone       healthState = iota // not in the pool this round
	hsUnknown                       // loaded, no completed ping yet
	hsAlive                         // latest observatory round had a success
	hsDead                          // every ping of the latest round failed
	hsParked                        // left out of the pool by the parker
	hsUplinkDown                    // nothing anywhere alive: the uplink, not the node
)

type healthRole byte

const (
	roleIdle healthRole = iota
	roleActive
	roleSpare
)

func (r healthRole) String() string {
	switch r {
	case roleActive:
		return "active"
	case roleSpare:
		return "spare"
	}
	return "idle"
}

type nodeSample struct {
	state healthState
	role  healthRole
	rttMs int32
}

type healthRound struct {
	at         time.Time
	uplinkDown bool
	ranked     bool // the pool had a daemon pick this round (roles are meaningful)
	nodes      map[string]nodeSample
}

type poolHistory struct {
	masters  []string
	strategy string
	auto     bool
	rounds   []healthRound // oldest → newest
}

// nodeHistory keeps the last nodeHistoryWindow of rounds per pool, keyed by
// the pool's dialer group key (stable across slot renumbering). Nil-safe.
type nodeHistory struct {
	mu    sync.Mutex
	now   func() time.Time
	pools map[string]*poolHistory
}

func newNodeHistory() *nodeHistory {
	return &nodeHistory{now: time.Now, pools: map[string]*poolHistory{}}
}

// record folds one poll into the history. slots are the loaded pools (parked
// members are absent from them), parked the parker's current set.
func (h *nodeHistory) record(slots []xray.Slot, byTag map[string]nodeStatus, parked map[string]time.Time) {
	if h == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	now := h.now()

	anyAlive, anySampled := false, false
	for _, sl := range slots {
		for _, m := range sl.Members {
			if st, ok := byTag[xray.SlotMemberTag(sl.Index, m.Key)]; ok && st.HealthPing.All > 0 {
				anySampled = true
				anyAlive = anyAlive || st.Alive
			}
		}
	}
	uplinkDown := anySampled && !anyAlive

	loaded := make(map[string]bool, len(slots))
	for _, sl := range slots {
		loaded[sl.Key] = true
		ph := h.pools[sl.Key]
		if ph == nil {
			ph = &poolHistory{}
			h.pools[sl.Key] = ph
		}
		ph.masters = sl.SlotMasters()
		ph.strategy = sl.Strategy
		ph.auto = sl.Auto

		// Active for any master on the pool beats spare for another.
		roles := map[string]healthRole{}
		for k := range sl.PickedKeys() {
			roles[k] = roleSpare
		}
		for k := range sl.ActiveKeys() {
			roles[k] = roleActive
		}
		r := healthRound{at: now, uplinkDown: uplinkDown, ranked: sl.Ranked(), nodes: make(map[string]nodeSample, len(sl.Members))}
		for _, m := range sl.Members {
			s := nodeSample{state: hsUnknown, role: roles[m.Key]}
			if st, ok := byTag[xray.SlotMemberTag(sl.Index, m.Key)]; ok && st.HealthPing.All > 0 {
				switch {
				case st.Alive:
					s.state, s.rttMs = hsAlive, int32(st.Delay)
				case uplinkDown:
					s.state = hsUplinkDown
				default:
					s.state = hsDead
				}
			}
			r.nodes[m.Key] = s
		}
		// Members seen in this pool before and now parked stay on the strip.
		for _, k := range ph.knownKeys() {
			if _, in := r.nodes[k]; in {
				continue
			}
			if _, ok := parked[k]; ok {
				r.nodes[k] = nodeSample{state: hsParked}
			}
		}
		ph.rounds = append(ph.rounds, r)
		ph.trim(now.Add(-nodeHistoryWindow))
	}
	for k := range h.pools {
		if !loaded[k] {
			delete(h.pools, k)
		}
	}
}

// knownKeys returns every member key in the pool's history.
func (ph *poolHistory) knownKeys() []string {
	seen := map[string]bool{}
	var out []string
	for _, r := range ph.rounds {
		for k := range r.nodes {
			if !seen[k] {
				seen[k] = true
				out = append(out, k)
			}
		}
	}
	return out
}

func (ph *poolHistory) trim(cutoff time.Time) {
	i := 0
	for i < len(ph.rounds) && ph.rounds[i].at.Before(cutoff) {
		i++
	}
	if i > 0 {
		ph.rounds = append([]healthRound(nil), ph.rounds[i:]...)
	}
}

// rename follows an entry or subscription rename: rekey maps an old pool key
// to its new one; a renamed entry member moves from oldMember to newMember.
func (h *nodeHistory) rename(rekey func(string) string, oldMember, newMember string) {
	if h == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	pools := make(map[string]*poolHistory, len(h.pools))
	for k, ph := range h.pools {
		if oldMember != "" {
			for _, r := range ph.rounds {
				if s, ok := r.nodes[oldMember]; ok {
					delete(r.nodes, oldMember)
					r.nodes[newMember] = s
				}
			}
		}
		pools[rekey(k)] = ph
	}
	h.pools = pools
}

// NodeHealth is one member's timeline and stats over a window.
type NodeHealth struct {
	Key       string
	Name      string        // human name (set by Supervisor.PoolHealth)
	Role      string        // latest round: active | spare | idle | parked | gone
	States    []healthState // one per round, oldest → newest
	UptimePct int           // alive share of the rounds it was alive or dead; -1 = no data
	Flips     int           // alive↔dead transitions
	AvgRTTms  int           // mean RTT over alive rounds; 0 = none
	LastRTTms int           // RTT of the latest alive round
}

// PoolHealth is one pool's timeline and stats over a window.
type PoolHealth struct {
	Key      string // dialer group key (the refs the pool serves)
	Masters  []string
	Strategy string // effective switch strategy at the latest round
	Auto     bool   // Strategy chosen by the auto classifier
	// Set by Supervisor.PoolHealth for auto pools: when the current mode
	// began and why.
	AutoSince  time.Time
	AutoReason string
	Since      time.Time // first round in the window
	Rounds     int
	PickLoss   int // times the whole pick (active + spare) went dead at once
	LossSec    int // time spent with the whole pick dead
	ChurnPct   int // share of observed members that flipped at least once
	Nodes      []NodeHealth
}

// snapshot returns every pool's timeline and stats over the last window,
// sorted by first master. window ≤ 0 or beyond nodeHistoryWindow means all.
func (h *nodeHistory) snapshot(window time.Duration) []PoolHealth {
	if h == nil {
		return nil
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if window <= 0 || window > nodeHistoryWindow {
		window = nodeHistoryWindow
	}
	cutoff := h.now().Add(-window)
	var out []PoolHealth
	for key, ph := range h.pools {
		i := 0
		for i < len(ph.rounds) && ph.rounds[i].at.Before(cutoff) {
			i++
		}
		rounds := ph.rounds[i:]
		if len(rounds) == 0 {
			continue
		}
		ps := poolStats(key, ph.masters, rounds)
		ps.Strategy, ps.Auto = ph.strategy, ph.auto
		out = append(out, ps)
	}
	sort.Slice(out, func(i, j int) bool {
		return strings.Join(out[i].Masters, ",") < strings.Join(out[j].Masters, ",")
	})
	return out
}

// poolStats computes a pool's timeline and stats over the given rounds.
func poolStats(key string, masters []string, rounds []healthRound) PoolHealth {
	p := PoolHealth{Key: key, Masters: append([]string(nil), masters...), Since: rounds[0].at, Rounds: len(rounds)}

	// Pick loss: rounds where the ranked pick had members and all were dead.
	inLoss := false
	for i, r := range rounds {
		loss := false
		if r.ranked && !r.uplinkDown {
			picked, dead := 0, 0
			for _, s := range r.nodes {
				if s.role == roleActive || s.role == roleSpare {
					picked++
					if s.state == hsDead {
						dead++
					}
				}
			}
			loss = picked > 0 && dead == picked
		}
		if loss {
			if !inLoss {
				p.PickLoss++
			}
			if i+1 < len(rounds) {
				p.LossSec += int(rounds[i+1].at.Sub(r.at).Seconds())
			} else {
				p.LossSec += int(nodeHealthPollInterval.Seconds())
			}
		}
		inLoss = loss
	}

	keys := map[string]bool{}
	for _, r := range rounds {
		for k := range r.nodes {
			keys[k] = true
		}
	}
	observed, flipped := 0, 0
	for k := range keys {
		n := NodeHealth{Key: k, States: make([]healthState, len(rounds)), UptimePct: -1}
		alive, dead := 0, 0
		var rttSum int64
		var last healthState
		for i, r := range rounds {
			s, ok := r.nodes[k]
			if !ok {
				continue
			}
			n.States[i] = s.state
			switch s.state {
			case hsAlive:
				alive++
				rttSum += int64(s.rttMs)
				n.LastRTTms = int(s.rttMs)
			case hsDead:
				dead++
			default:
				continue
			}
			if last != hsNone && last != s.state {
				n.Flips++
			}
			last = s.state
		}
		if alive+dead > 0 {
			n.UptimePct = alive * 100 / (alive + dead)
			observed++
			if n.Flips > 0 {
				flipped++
			}
		}
		if alive > 0 {
			n.AvgRTTms = int(rttSum / int64(alive))
		}
		latest := rounds[len(rounds)-1]
		if s, ok := latest.nodes[k]; !ok {
			n.Role = "gone"
		} else if s.state == hsParked {
			n.Role = "parked"
		} else {
			n.Role = s.role.String()
		}
		p.Nodes = append(p.Nodes, n)
	}
	if observed > 0 {
		p.ChurnPct = flipped * 100 / observed
	}
	sort.Slice(p.Nodes, func(i, j int) bool {
		a, b := p.Nodes[i], p.Nodes[j]
		if ra, rb := roleRank(a.Role), roleRank(b.Role); ra != rb {
			return ra < rb
		}
		if a.UptimePct != b.UptimePct {
			return a.UptimePct > b.UptimePct
		}
		return a.Key < b.Key
	})
	return p
}

func roleRank(role string) int {
	switch role {
	case "active":
		return 0
	case "spare":
		return 1
	case "idle":
		return 2
	case "parked":
		return 3
	}
	return 4
}

// PoolHealth returns every loaded pool's health timeline over window.
func (s *Supervisor) PoolHealth(window time.Duration) []PoolHealth {
	out := s.history.snapshot(window)
	for i := range out {
		if st, ok := s.auto.state(out[i].Key); ok && out[i].Auto {
			out[i].AutoSince, out[i].AutoReason = st.since, st.reason
		}
		for j := range out[i].Nodes {
			out[i].Nodes[j].Name = s.memberName(out[i].Nodes[j].Key)
		}
	}
	return out
}
