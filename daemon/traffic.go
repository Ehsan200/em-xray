package daemon

import (
	"context"
	"log"
	"time"

	"github.com/ehsan200/em-xray/core/xray"
)

// trafficSampleTick is how often xray's cumulative counters are sampled into
// hourly buckets. One minute gives smooth 24h charts without much overhead.
const trafficSampleTick = time.Minute

// TrafficSampler polls xray's per-tag byte counters and accumulates them into
// the store as hourly deltas + lifetime totals. Counters are cumulative since
// xray started (which, since xray is our child, coincides with daemon start),
// so the first poll's value IS the delta; a counter going backwards means xray
// restarted and we take the new value as the delta.
type TrafficSampler struct {
	store     *xray.Store
	sup       *Supervisor
	log       *log.Logger
	last      map[string]int64 // "kind|tag|dir" -> last raw cumulative
	lastPrune int64
}

func NewTrafficSampler(store *xray.Store, sup *Supervisor, logger *log.Logger) *TrafficSampler {
	return &TrafficSampler{store: store, sup: sup, log: logger, last: map[string]int64{}}
}

// Start samples immediately, then every trafficSampleTick until ctx is done.
func (t *TrafficSampler) Start(ctx context.Context) {
	go func() {
		tk := time.NewTicker(trafficSampleTick)
		defer tk.Stop()
		t.poll()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tk.C:
				t.poll()
			}
		}
	}()
}

// poll reads counters and writes deltas. It is a no-op (silently) when xray is
// down or the api isn't reachable yet.
func (t *TrafficSampler) poll() {
	counters, err := t.sup.StatsQuery()
	if err != nil {
		return // xray not up / no api — try again next tick
	}
	idx := t.tagIndex()
	hour := xray.HourFloor(time.Now())

	type agg struct {
		up, down int64
		name     string
	}
	byTag := map[string]*agg{}
	for _, c := range counters {
		cur := c.Kind + "|" + c.Tag + "|" + dirKey(c.Down)
		delta := c.Bytes - t.last[cur]
		if delta < 0 {
			delta = c.Bytes // counter reset (xray restart)
		}
		t.last[cur] = c.Bytes

		name, keep := idx.lookup(c.Kind, c.Tag)
		if !keep {
			continue // cursor advanced above; just not stored
		}
		key := c.Kind + "|" + c.Tag
		a := byTag[key]
		if a == nil {
			a = &agg{name: name}
			byTag[key] = a
		}
		if c.Down {
			a.down += delta
		} else {
			a.up += delta
		}
	}

	for key, a := range byTag {
		if a.up == 0 && a.down == 0 {
			continue
		}
		kind, tag := splitKey(key)
		if err := t.store.AddTraffic(kind, tag, a.name, hour, a.up, a.down); err != nil {
			t.log.Printf("traffic: store %s failed: %v", key, err)
		}
	}

	// Prune stale buckets once per hour.
	if hour != t.lastPrune {
		cut := xray.HourFloor(time.Now().Add(-xray.TrafficRetention))
		if err := t.store.PruneTrafficBefore(cut); err != nil {
			t.log.Printf("traffic: prune failed: %v", err)
		}
		t.lastPrune = hour
	}
}

// tagIndex maps live inbound/outbound tags back to display names, and decides
// which tags to keep (user inbounds, user entries/masters, and direct egress).
type tagIndex struct {
	in  map[string]string
	out map[string]string
}

func (t *TrafficSampler) tagIndex() tagIndex {
	idx := tagIndex{in: map[string]string{}, out: map[string]string{"direct": "direct"}}
	if ins, err := t.store.ListInbounds(); err == nil {
		for _, in := range ins {
			idx.in[xray.InboundTag(in.Name)] = in.Name
		}
	}
	if ents, err := t.store.ListEntries(); err == nil {
		for _, e := range ents {
			idx.out[xray.OutboundTag(e.Name)] = e.Name
		}
	}
	return idx
}

func (i tagIndex) lookup(kind, tag string) (string, bool) {
	if kind == xray.KindInbound {
		n, ok := i.in[tag]
		return n, ok
	}
	n, ok := i.out[tag]
	return n, ok
}

func dirKey(down bool) string {
	if down {
		return "d"
	}
	return "u"
}

func splitKey(key string) (kind, tag string) {
	for i := 0; i < len(key); i++ {
		if key[i] == '|' {
			return key[:i], key[i+1:]
		}
	}
	return key, ""
}
