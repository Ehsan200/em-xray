package daemon

import (
	"fmt"
	"log"
	"os"
	"os/exec"
	"sync"
	"time"

	"github.com/ehsan200/em-xray/core/xray"
	"github.com/ehsan200/em-xray/internal/paths"
	"github.com/ehsan200/em-xray/internal/xraybin"
)

// Supervisor owns the xray child (via the Watchdog) and the config it runs.
// Reconcile regenerates config.json and (re)starts xray; later phases add
// SyncDialerMembers for zero-restart member churn.
type Supervisor struct {
	store *xray.Store
	paths paths.Paths
	log   *log.Logger
	wd    *Watchdog

	mu sync.Mutex
	// loadedSlots is what xray currently has live per master: slot index + the
	// set of member keys. Set to the baked slots on Reconcile; advanced
	// incrementally by SyncDialerMembers as it applies deltas.
	loadedSlots map[string]loadedSlot
}

// loadedSlot tracks a master's live slot: its index and current member keys.
type loadedSlot struct {
	idx  int
	keys map[string]bool
}

func NewSupervisor(store *xray.Store, p paths.Paths, logger *log.Logger) *Supervisor {
	s := &Supervisor{store: store, paths: p, log: logger, loadedSlots: map[string]loadedSlot{}}
	s.wd = NewWatchdog(xrayCmdFactory(p, logger), logger)
	return s
}

// Reconcile is the full path: assign ports, regenerate config.json, and start or
// restart xray. Restart-on-config-change is acceptable here — the zero-restart
// requirement applies to routine node churn (SyncDialerMembers), not structural
// changes. Safe to call from multiple goroutines (serialized by mu).
func (s *Supervisor) Reconcile() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.reconcileLocked()
}

// reconcileLocked is Reconcile's body; callers must hold s.mu.
// activeUsers returns an inbound's users that should currently have access:
// enabled, and either uncapped or still under their lifetime byte cap.
func (s *Supervisor) activeUsers(inboundID uint) []xray.InboundUser {
	users, err := s.store.InboundUsers(inboundID)
	if err != nil {
		return nil
	}
	var out []xray.InboundUser
	for _, u := range users {
		if !u.Enabled {
			continue
		}
		if u.ByteCap > 0 {
			up, down := s.store.TrafficTotalFor(xray.KindUser, u.Email)
			if up+down >= u.ByteCap {
				continue // over quota → deny
			}
		}
		out = append(out, u)
	}
	return out
}

func (s *Supervisor) reconcileLocked() error {
	entries, err := s.store.ListEntries()
	if err != nil {
		return err
	}
	inbounds, err := s.store.ListInbounds()
	if err != nil {
		return err
	}
	// Allocate + persist any missing inbound listen ports.
	for _, i := range xray.AssignInboundPorts(inbounds) {
		if err := s.store.UpdateInbound(&inbounds[i]); err != nil {
			return err
		}
	}
	// Attach each inbound's active users (enabled + under byte cap) so Generate
	// emits their clients. Over-quota users are dropped → access denied.
	for i := range inbounds {
		inbounds[i].Users = s.activeUsers(inbounds[i].ID)
	}

	slots, err := s.resolveDialerSlots(entries)
	if err != nil {
		return err
	}

	cfg, err := xray.Generate(entries, inbounds, slots, xray.GenOptions{
		AccessLog: s.paths.AccessLog(),
		ErrorLog:  s.paths.ErrorLog(),
		LogLevel:  s.store.LogLevel(),
	})
	if err != nil {
		return err
	}
	if err := writeFileAtomic(s.paths.XrayConfig(), cfg); err != nil {
		return err
	}

	if routable(inbounds) {
		if s.wd.IsStarted() {
			s.log.Print("reconcile: restarting xray with new config")
			s.wd.Restart()
		} else {
			s.log.Print("reconcile: starting xray")
			s.wd.Start()
		}
		// The freshly-written config bakes exactly these members, so they are
		// now what xray has live. Live sync diffs against this baseline.
		s.loadedSlots = toLoaded(slots)
	} else if s.wd.IsStarted() {
		s.log.Print("reconcile: no routable entries — stopping xray")
		s.wd.Stop(5 * time.Second)
		s.loadedSlots = map[string]loadedSlot{}
	} else {
		s.loadedSlots = map[string]loadedSlot{}
	}
	return nil
}

// toLoaded snapshots resolved slots into the live-tracking form.
func toLoaded(slots []xray.Slot) map[string]loadedSlot {
	idx := xray.SlotIndexByName(slots)
	m := make(map[string]loadedSlot, len(slots))
	for _, sl := range slots {
		keys := make(map[string]bool, len(sl.Members))
		for _, mem := range sl.Members {
			keys[mem.Key] = true
		}
		m[sl.Master] = loadedSlot{idx: idx[sl.Master], keys: keys}
	}
	return m
}

// Stop terminates the xray child and its supervisor loop.
func (s *Supervisor) Stop() { s.wd.Stop(5 * time.Second) }

// XrayState exposes child health for the Status RPC.
func (s *Supervisor) XrayState() (running bool, pid int, restarts int32, lastErr string) {
	return s.wd.State()
}

// resolveDialerSlots resolves each enabled master's Dialer refs into concrete
// pool members right now: xray: refs → the named entry's outbound; xraysub:
// refs → the subscription's ACTIVE nodes. Members dedupe by content Key. A
// master resolving to zero members gets no slot (it routes direct until a
// refresh populates its pool). proxy: refs are not yet supported.
func (s *Supervisor) resolveDialerSlots(entries []xray.XrayEntry) ([]xray.Slot, error) {
	byName := make(map[string]xray.XrayEntry, len(entries))
	for _, e := range entries {
		byName[e.Name] = e
	}

	var slots []xray.Slot
	for _, e := range entries {
		if !e.Enabled || !e.IsMaster() {
			continue
		}
		refs, err := xray.ParseDialer(e.Dialer)
		if err != nil {
			s.log.Printf("master %q: bad dialer: %v", e.Name, err)
			continue
		}
		var members []xray.SlotMember
		seen := map[string]bool{}
		add := func(key, outbound string) {
			if key == "" || outbound == "" || seen[key] {
				return
			}
			seen[key] = true
			members = append(members, xray.SlotMember{Key: key, Outbound: outbound})
		}
		for _, r := range refs {
			switch r.Kind {
			case xray.RefXray:
				if m, ok := byName[r.Name]; ok && m.Enabled {
					add("xray-"+r.Name, m.Outbound)
				}
			case xray.RefXraySub:
				sub, err := s.store.GetSubscriptionByName(r.Name)
				if err != nil {
					continue
				}
				nodes, err := s.store.ActiveNodes(sub.ID)
				if err != nil {
					continue
				}
				for _, n := range nodes {
					add(n.Fingerprint, n.Outbound)
				}
			case xray.RefProxy:
				// proxy upstreams not yet modelled — skipped.
			}
		}
		if len(members) > 0 {
			slots = append(slots, xray.Slot{Master: e.Name, Members: members})
		}
	}
	return slots, nil
}

// routable reports whether xray has anything to serve: any enabled inbound with
// a listen port.
func routable(inbounds []xray.Inbound) bool {
	for _, in := range inbounds {
		if in.Enabled && in.Port != 0 {
			return true
		}
	}
	return false
}

// xrayCmdFactory builds a `xray run -c <config>` command with the embedded
// binary extracted and the geo asset dir wired.
func xrayCmdFactory(p paths.Paths, logger *log.Logger) CmdFactory {
	return func() (*exec.Cmd, error) {
		bin, err := xraybin.Extract(p.Cache)
		if err != nil {
			return nil, err
		}
		if _, err := os.Stat(p.XrayConfig()); err != nil {
			return nil, fmt.Errorf("no xray config yet: %w", err)
		}
		cmd := exec.Command(bin, "run", "-c", p.XrayConfig())
		cmd.Env = append(os.Environ(), "XRAY_LOCATION_ASSET="+p.AssetDir())
		// xray writes its own access/error logs (paths baked into config); its
		// stdout/stderr go to the daemon error log for startup diagnostics.
		if errLog, err := os.OpenFile(p.ErrorLog(), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644); err == nil {
			cmd.Stdout = errLog
			cmd.Stderr = errLog
		}
		return cmd, nil
	}
}

// writeFileAtomic writes via a temp file + rename so a reading xray never sees a
// half-written config.
func writeFileAtomic(path string, data []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
