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
	overCap   map[string]bool  // user emails already reconciled out for cap
	lastPrune int64
}

func NewTrafficSampler(store *xray.Store, sup *Supervisor, logger *log.Logger) *TrafficSampler {
	return &TrafficSampler{store: store, sup: sup, log: logger, last: map[string]int64{}, overCap: map[string]bool{}}
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

	t.enforceCaps()

	// Prune stale buckets once per hour, per the configured retention.
	if hour != t.lastPrune {
		retention := time.Duration(t.store.TrafficDays()) * 24 * time.Hour
		cut := xray.HourFloor(time.Now().Add(-retention))
		if err := t.store.PruneTrafficBefore(cut); err != nil {
			t.log.Printf("traffic: prune failed: %v", err)
		}
		t.lastPrune = hour
	}
}

// tagIndex maps live inbound/outbound tags back to display names, and decides
// which tags to keep (user inbounds, user entries/masters, and direct egress).
type tagIndex struct {
	in   map[string]string
	out  map[string]string
	user map[string]string
}

func (t *TrafficSampler) tagIndex() tagIndex { return buildTagIndex(t.store) }

// buildTagIndex maps live inbound/outbound/user tags back to display names,
// keeping user inbounds, user entries/masters, direct egress, and per-user
// (per-client) email tags.
func buildTagIndex(store *xray.Store) tagIndex {
	idx := tagIndex{
		in:   map[string]string{},
		out:  map[string]string{"direct": "direct"},
		user: map[string]string{},
	}
	if ins, err := store.ListInbounds(); err == nil {
		for _, in := range ins {
			idx.in[xray.InboundTag(in.Name)] = in.Name
			email := in.ClientEmail
			if email == "" {
				email = xray.PrimaryUserEmail(in.Name)
			}
			idx.user[email] = in.Name // primary client email
			if us, err := store.InboundUsers(in.ID); err == nil {
				for _, u := range us {
					idx.user[u.Email] = in.Name + "/" + u.Name
				}
			}
		}
	}
	if ents, err := store.ListEntries(); err == nil {
		for _, e := range ents {
			idx.out[xray.OutboundTag(e.Name)] = e.Name
		}
	}
	return idx
}

func (i tagIndex) lookup(kind, tag string) (string, bool) {
	switch kind {
	case xray.KindInbound:
		n, ok := i.in[tag]
		return n, ok
	case xray.KindUser:
		n, ok := i.user[tag]
		return n, ok
	default:
		n, ok := i.out[tag]
		return n, ok
	}
}

// enforceCaps denies users whose lifetime usage has reached their byte cap by
// triggering a reconcile (which drops them from the config). It reconciles only
// on the transition into over-cap, so it doesn't churn every poll.
func (t *TrafficSampler) enforceCaps() {
	ins, err := t.store.ListInbounds()
	if err != nil {
		return
	}
	trigger := false
	for _, in := range ins {
		users, err := t.store.InboundUsers(in.ID)
		if err != nil {
			continue
		}
		for _, u := range users {
			if !u.Enabled || u.ByteCap <= 0 {
				continue
			}
			up, down := t.store.TrafficTotalFor(xray.KindUser, u.Email)
			over := up+down >= u.ByteCap
			switch {
			case over && !t.overCap[u.Email]:
				t.overCap[u.Email] = true
				trigger = true
				t.log.Printf("user %q over byte cap — denying access", in.Name+"/"+u.Name)
			case !over && t.overCap[u.Email]:
				delete(t.overCap, u.Email) // cap raised/reset → re-admit next reconcile
				trigger = true
			}
		}
	}
	if trigger {
		if err := t.sup.Reconcile(); err != nil {
			t.log.Printf("traffic: cap reconcile failed: %v", err)
		}
	}
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
