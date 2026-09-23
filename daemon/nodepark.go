package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/ehsan200/em-xray/core/xray"
)

// Parking dead pool nodes.
//
// A subscription pool routinely carries nodes that are simply gone. The burst
// observatory keeps pinging each of them every interval forever — a full
// outbound dial through the node and a warning line in xray's error log, per
// dead node per interval — and leastLoad already routes around them, so they
// cost probes and log volume and buy nothing.
//
// A node that has failed every ping for nodeDeadBeforePark is parked: left out
// of its slot's members, which the live apply turns into a plain outbound
// removal — no restart, nothing else touched. After its park time it is put
// back on trial; one live ping clears its history, and failing through the
// trial parks it again for twice as long (capped). Parking never empties a
// pool and only happens while another member of the same slot is alive: if
// every node looks dead, the likelier cause is this box's own uplink, and
// parking would turn a local outage into a pool that stays empty after the
// link returns. State is in memory; a daemon restart re-trials everything.

const (
	nodeHealthPollInterval = 30 * time.Second
	nodeDeadBeforePark     = 15 * time.Minute
	nodeTrialWindow        = 3 * time.Minute
	nodeParkInitial        = 30 * time.Minute
	nodeParkMax            = 6 * time.Hour
)

// nodeStatus is one outbound's entry in xray's /debug/vars "observatory".
type nodeStatus struct {
	Alive      bool `json:"alive"`
	HealthPing struct {
		All  int `json:"all"`
		Fail int `json:"fail"`
	} `json:"health_ping"`
}

// dead reports a node whose every recent ping failed. A node with no pings yet
// is unknown, not dead.
func (n nodeStatus) dead() bool {
	return !n.Alive && n.HealthPing.All > 0 && n.HealthPing.Fail == n.HealthPing.All
}

// parseObservatoryVars extracts tag → status from a /debug/vars body.
func parseObservatoryVars(b []byte) (map[string]nodeStatus, error) {
	var vars struct {
		Observatory map[string]nodeStatus `json:"observatory"`
	}
	if err := json.Unmarshal(b, &vars); err != nil {
		return nil, err
	}
	return vars.Observatory, nil
}

type parkState struct {
	deadSince   time.Time     // first consecutive dead observation; zero = not dead
	parkedUntil time.Time     // zero = not parked
	parkFor     time.Duration // next park length (doubles per failed trial)
	onTrial     bool          // just returned from parking
}

// nodeParker decides which pool members (by SlotMember.Key) are parked. All
// methods are safe on a nil receiver, which parks nothing.
type nodeParker struct {
	mu    sync.Mutex
	now   func() time.Time
	nodes map[string]*parkState
}

func newNodeParker() *nodeParker {
	return &nodeParker{now: time.Now, nodes: map[string]*parkState{}}
}

// parked returns the keys currently parked with their release time.
func (p *nodeParker) parked() map[string]time.Time {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.now()
	out := map[string]time.Time{}
	for k, st := range p.nodes {
		if now.Before(st.parkedUntil) {
			out[k] = st.parkedUntil
		}
	}
	return out
}

// parkEvent describes one decision, for logging.
type parkEvent struct {
	key    string
	parked bool // false = returned on trial
	for_   time.Duration
}

// observe folds one round of health into the park state and returns what
// changed. slots are the members currently loaded (parked ones are absent, so
// they are never observed while parked).
func (p *nodeParker) observe(slots []xray.Slot, byTag map[string]nodeStatus) []parkEvent {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.now()
	var events []parkEvent

	// Forget nodes that left every pool (a refresh dropped them) unless they
	// are parked — parked nodes are absent from the slots by design.
	loaded := map[string]bool{}
	for _, sl := range slots {
		for _, m := range sl.Members {
			loaded[m.Key] = true
		}
	}
	for k, st := range p.nodes {
		if !loaded[k] && st.parkedUntil.IsZero() {
			delete(p.nodes, k)
		}
	}

	for _, sl := range slots {
		status := make(map[string]nodeStatus, len(sl.Members))
		aliveCount := 0
		for _, m := range sl.Members {
			st, ok := byTag[xray.SlotMemberTag(sl.Index, m.Key)]
			if !ok {
				continue
			}
			status[m.Key] = st
			if st.Alive {
				aliveCount++
			}
		}
		remaining := len(sl.Members)
		for _, m := range sl.Members {
			st, ok := status[m.Key]
			if !ok {
				continue
			}
			ps := p.nodes[m.Key]
			switch {
			case st.Alive:
				delete(p.nodes, m.Key) // healthy: forget any history
			case st.dead():
				if ps == nil {
					ps = &parkState{}
					p.nodes[m.Key] = ps
				}
				if ps.deadSince.IsZero() {
					ps.deadSince = now
				}
				wait := nodeDeadBeforePark
				if ps.onTrial {
					wait = nodeTrialWindow
				}
				// Park only with a live sibling (uplink demonstrably up) and
				// never the pool's last member.
				if now.Sub(ps.deadSince) < wait || aliveCount == 0 || remaining <= 1 {
					continue
				}
				switch {
				case ps.parkFor == 0:
					ps.parkFor = nodeParkInitial
				case ps.onTrial:
					ps.parkFor *= 2
					if ps.parkFor > nodeParkMax {
						ps.parkFor = nodeParkMax
					}
				}
				ps.parkedUntil = now.Add(ps.parkFor)
				ps.deadSince = time.Time{}
				ps.onTrial = false
				remaining--
				events = append(events, parkEvent{key: m.Key, parked: true, for_: ps.parkFor})
			}
		}
	}
	return events
}

// release returns parked nodes whose time is up to the pool, on trial.
func (p *nodeParker) release() []parkEvent {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.now()
	var events []parkEvent
	for k, st := range p.nodes {
		if !st.parkedUntil.IsZero() && !now.Before(st.parkedUntil) {
			st.parkedUntil = time.Time{}
			st.onTrial = true
			st.deadSince = time.Time{}
			events = append(events, parkEvent{key: k})
		}
	}
	sort.Slice(events, func(i, j int) bool { return events[i].key < events[j].key })
	return events
}

// withoutParked drops parked members from a resolved member list, unless that
// would leave it empty.
func withoutParked(members []xray.SlotMember, parked map[string]time.Time) []xray.SlotMember {
	if len(parked) == 0 {
		return members
	}
	out := make([]xray.SlotMember, 0, len(members))
	for _, m := range members {
		if _, ok := parked[m.Key]; !ok {
			out = append(out, m)
		}
	}
	if len(out) == 0 {
		return members
	}
	return out
}

// ParkedNodes reports the pool members currently parked, key → release time.
func (s *Supervisor) ParkedNodes() map[string]time.Time { return s.parker.parked() }

// StartNodeHealth polls per-node health every nodeHealthPollInterval.
func (s *Supervisor) StartNodeHealth(ctx context.Context) {
	go func() {
		tk := time.NewTicker(nodeHealthPollInterval)
		defer tk.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tk.C:
				s.PollNodeHealth(ctx)
			}
		}
	}()
}

// PollNodeHealth reads per-node health from xray's metrics endpoint, parks
// nodes that have stayed dead, returns parked nodes whose time is up, and
// applies any change live (a member removal or re-add).
func (s *Supervisor) PollNodeHealth(ctx context.Context) {
	s.mu.Lock()
	running := s.wd.IsStarted() && s.running != nil
	slots := s.loadedSlots
	s.mu.Unlock()
	if !running {
		return
	}
	var events []parkEvent
	if len(slots) > 0 {
		byTag, err := fetchObservatory(ctx, "127.0.0.1:"+strconv.Itoa(xray.MetricsPort))
		if err != nil {
			return // metrics not up yet (first seconds after a start)
		}
		events = s.parker.observe(slots, byTag)
	}
	events = append(events, s.parker.release()...)
	if len(events) == 0 {
		return
	}
	for _, e := range events {
		name := s.memberName(e.key)
		if e.parked {
			s.log.Printf("pool node %s dead for %s — parked for %s", name, nodeDeadBeforePark, e.for_)
		} else {
			s.log.Printf("pool node %s back on trial", name)
		}
	}
	if err := s.Reconcile(); err != nil {
		s.log.Printf("apply node parking: %v", err)
	}
}

func fetchObservatory(ctx context.Context, addr string) (map[string]nodeStatus, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+addr+"/debug/vars", nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("metrics: %s", resp.Status)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, err
	}
	return parseObservatoryVars(b)
}
