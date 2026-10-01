package daemon

import (
	"fmt"
	"sync"
	"time"

	"github.com/ehsan200/em-xray/core/xray"
)

// The auto strategy.
//
// A pool whose subscriptions leave the strategy on auto is run stable until
// its health history shows it flapping — nodes coming and going in waves —
// and agile from then on, until it has been calm for a while. Judged every
// poll over the last AutoTuning.WindowMin minutes (nodehistory.go), with
// hysteresis: quick to go agile (a flapping pool loses traffic every wave
// while it stays stable), slow to go back (CalmMin without flapping), so a
// pool on the edge doesn't bounce between the two. State is in memory; a
// daemon restart starts every auto pool stable again.

// autoMinFlapping is the fewest flapping nodes that make a pool flapping by
// share: one flaky node in a small pool is not a wave.
const autoMinFlapping = 2

type autoState struct {
	agile      bool
	since      time.Time // when the current mode began
	lastFlappy time.Time // latest poll that judged the pool flapping
	reason     string    // why the current mode was chosen
}

// autoClassifier keeps each auto pool's mode by pool key. Nil-safe.
type autoClassifier struct {
	mu    sync.Mutex
	now   func() time.Time
	pools map[string]*autoState
}

func newAutoClassifier() *autoClassifier {
	return &autoClassifier{now: time.Now, pools: map[string]*autoState{}}
}

// agile reports the mode the classifier holds for pool key (stable when the
// pool has not been judged yet).
func (c *autoClassifier) agile(key string) bool {
	if c == nil {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	st := c.pools[key]
	return st != nil && st.agile
}

// state returns a copy of pool key's state, if any.
func (c *autoClassifier) state(key string) (autoState, bool) {
	if c == nil {
		return autoState{}, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	st, ok := c.pools[key]
	if !ok {
		return autoState{}, false
	}
	return *st, true
}

// autoEvent is one mode switch, for logging.
type autoEvent struct {
	key    string
	agile  bool
	reason string
}

// flapping judges one pool's window of health against the thresholds.
func flapping(p PoolHealth, t xray.AutoTuning) (bool, string) {
	win := time.Duration(t.WindowMin) * time.Minute
	if t.PickLoss > 0 && p.PickLoss >= t.PickLoss {
		return true, fmt.Sprintf("whole pick died %d× in %s", p.PickLoss, shortMinutes(win))
	}
	observed, flappers := 0, 0
	for _, n := range p.Nodes {
		if n.UptimePct < 0 {
			continue
		}
		observed++
		if n.Flips >= t.Flips {
			flappers++
		}
	}
	if flappers >= autoMinFlapping && flappers*100 >= t.FlappingPct*observed {
		return true, fmt.Sprintf("%d/%d nodes flipped alive↔dead %d+ times in %s", flappers, observed, t.Flips, shortMinutes(win))
	}
	return false, ""
}

// evaluate judges the auto pools (by key) from their health windows and
// returns the switches it made. Pools no longer auto are forgotten.
func (c *autoClassifier) evaluate(windows []PoolHealth, auto map[string]bool, t xray.AutoTuning) []autoEvent {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	for k := range c.pools {
		if !auto[k] {
			delete(c.pools, k)
		}
	}
	calm := time.Duration(t.CalmMin) * time.Minute
	var events []autoEvent
	for _, p := range windows {
		if !auto[p.Key] {
			continue
		}
		st := c.pools[p.Key]
		if st == nil {
			st = &autoState{since: now, reason: "no flapping seen"}
			c.pools[p.Key] = st
		}
		flappy, why := flapping(p, t)
		switch {
		case flappy:
			st.lastFlappy = now
			if !st.agile {
				st.agile, st.since, st.reason = true, now, why
				events = append(events, autoEvent{key: p.Key, agile: true, reason: why})
			}
		case st.agile && now.Sub(st.lastFlappy) >= calm:
			why := "calm for " + shortMinutes(calm)
			st.agile, st.since, st.reason = false, now, why
			events = append(events, autoEvent{key: p.Key, reason: why})
		}
	}
	return events
}

// rename follows a pool key rename.
func (c *autoClassifier) rename(rekey func(string) string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	pools := make(map[string]*autoState, len(c.pools))
	for k, st := range c.pools {
		pools[rekey(k)] = st
	}
	c.pools = pools
}

func shortMinutes(d time.Duration) string {
	if d%time.Minute == 0 {
		return fmt.Sprintf("%dm", int(d/time.Minute))
	}
	return d.String()
}

// autoSwitched judges the loaded auto pools after a poll and reports whether
// any changed mode (the caller reconciles to apply it).
func (s *Supervisor) autoSwitched(slots []xray.Slot) bool {
	auto := map[string]bool{}
	masters := map[string]string{}
	for _, sl := range slots {
		if sl.Auto {
			auto[sl.Key] = true
			masters[sl.Key] = sl.Master
		}
	}
	t := s.store.AutoTuning()
	events := s.auto.evaluate(s.history.snapshot(time.Duration(t.WindowMin)*time.Minute), auto, t)
	for _, e := range events {
		mode := xray.StrategyStable
		if e.agile {
			mode = xray.StrategyAgile
		}
		s.log.Printf("pool %s (%s): auto → %s (%s)", masters[e.key], e.key, mode, e.reason)
	}
	return len(events) > 0
}
