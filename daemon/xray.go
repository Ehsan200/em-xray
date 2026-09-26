package daemon

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
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

	mu              sync.Mutex
	healthMu        sync.Mutex
	healthChecked   bool
	healthOK        bool
	healthFailures  int
	healthRestarts  int32
	healthMessage   string
	healthCheckedAt time.Time
	// running is the config the live process reflects — what it was started
	// with, advanced by every successful live apply. Reconcile diffs against it
	// to change the process without restarting it.
	running []byte
	// loadedSlots are the dialer slots in running (winner/balancer queries).
	loadedSlots []xray.Slot
	// slotIdx pins each pool (dialer group key) to its slot index across
	// reconciles, so adding a master that sorts first never renumbers — and
	// thereby replaces — every other pool's slot.
	slotIdx map[string]int
	// Config restarts of a running process vs. changes applied live. Atomic
	// so Status never waits on a reconcile holding mu.
	restarts    atomic.Int32
	liveApplies atomic.Int32
	// parker leaves dead pool nodes out of their slot (see nodepark.go).
	parker *nodeParker
	// rejected are pool members xray refused (see validate.go).
	rejectedMu sync.Mutex
	rejected   map[string]rejection
	// cfgErr is the last reason a generated config was refused.
	cfgErrMu sync.Mutex
	cfgErr   string
	// probeURL overrides the observatory ping destination (tests point it at
	// a local 204 server); "" = xray.DefaultProbeURL.
	probeURL string
}

const (
	xrayHealthInterval     = 30 * time.Second
	xrayHealthFailureLimit = 3
)

func NewSupervisor(store *xray.Store, p paths.Paths, logger *log.Logger) *Supervisor {
	s := &Supervisor{store: store, paths: p, log: logger, slotIdx: map[string]int{}, parker: newNodeParker(), rejected: map[string]rejection{}}
	s.wd = NewWatchdog(xrayCmdFactory(p, logger), logger)
	return s
}

// Reconcile is the one path from stored state to the running xray: assign
// ports, regenerate config.json, and start xray or bring it to the new config
// — live through the api whenever possible (see xray_live.go), restarting only
// when unavoidable. Safe to call from multiple goroutines (serialized by mu).
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
	err := s.reconcileOnceLocked()
	if err != nil {
		s.startLastGoodLocked()
	}
	return err
}

// startLastGoodLocked keeps xray up when the current state can't be applied
// (xray refuses the generated config, generation fails): if nothing is
// running, it starts xray on config.json — the last config that passed
// validation — rather than leaving the box without its listeners. The next
// successful reconcile brings it to current state like any other change.
func (s *Supervisor) startLastGoodLocked() {
	if s.wd.IsStarted() {
		return
	}
	inbounds, err := s.store.ListInbounds()
	if err != nil || !routable(inbounds) {
		return
	}
	cfg, err := os.ReadFile(s.paths.XrayConfig())
	if err != nil || len(bytes.TrimSpace(cfg)) == 0 {
		return
	}
	s.log.Print("reconcile: current config refused — starting xray on the last applied config")
	s.wd.Start()
	s.running, s.loadedSlots = cfg, nil
}

func (s *Supervisor) reconcileOnceLocked() error {
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
	// Generate, then have xray itself check the config before anything is
	// applied (see validate.go). A refused pool member is left out and the
	// config regenerated; anything else keeps the running config.
	for attempt := 0; ; attempt++ {
		slots, err := s.resolveDialerSlots(entries)
		if err != nil {
			return err
		}
		cfg, err := xray.Generate(entries, inbounds, slots, xray.GenOptions{
			AccessLog:     s.paths.AccessLog(),
			ErrorLog:      s.paths.ErrorLog(),
			LogLevel:      s.store.LogLevel(),
			ProbeInterval: strconv.Itoa(s.store.ProbeIntervalSec()) + "s",
			ProbeURL:      s.probeURL,
		})
		if err != nil {
			s.setConfigError(err.Error())
			return err
		}
		if s.running != nil && bytes.Equal(s.running, cfg) {
			// Already validated when it was applied.
			return s.applyLocked(inbounds, slots, cfg)
		}
		verr := s.testConfig(cfg)
		if verr == nil {
			s.setConfigError("")
			return s.applyLocked(inbounds, slots, cfg)
		}
		if m, ok := memberForTag(slots, rejectedTag(verr)); ok && attempt < 16 {
			s.log.Printf("xray rejects pool node %s — left out of its pool: %v", s.memberName(m.Key), verr)
			s.reject(m.Key, rejection{outbound: m.Outbound, reason: verr.Error()})
			continue
		}
		msg := fmt.Sprintf("xray rejects the new config, keeping the running one: %v", verr)
		s.log.Print(msg)
		s.setConfigError(msg)
		return fmt.Errorf("%s", msg)
	}
}

// memberForTag finds the pool member a slot member tag belongs to.
func memberForTag(slots []xray.Slot, tag string) (xray.SlotMember, bool) {
	for _, sl := range slots {
		for _, m := range sl.Members {
			if xray.SlotMemberTag(sl.Index, m.Key) == tag {
				return m, true
			}
		}
	}
	return xray.SlotMember{}, false
}

// applyLocked brings xray to cfg: start it, leave it alone (identical),
// apply the difference live through the api, or — only when the api can't
// express the change or a call fails — restart it. Callers hold s.mu.
func (s *Supervisor) applyLocked(inbounds []xray.Inbound, slots []xray.Slot, cfg []byte) error {
	if !routable(inbounds) {
		if s.wd.IsStarted() {
			s.log.Print("reconcile: no routable entries — stopping xray")
			s.wd.Stop(5 * time.Second)
		}
		s.running, s.loadedSlots = nil, nil
		return nil
	}
	// Generate is deterministic precisely so this comparison is possible: an
	// RPC that reconciles without changing anything must not touch xray.
	if s.wd.IsStarted() && s.running != nil && bytes.Equal(s.running, cfg) {
		s.loadedSlots = slots
		return nil
	}
	// The file is what the watchdog (re)starts xray from, so it is written
	// first: a crash, or a restart after a failed live apply, converges on it.
	if err := writeFileAtomic(s.paths.XrayConfig(), cfg); err != nil {
		return err
	}
	// A direct inbound egresses from this box's own IP. Legitimate, but never
	// something to discover by accident — name them whenever the config
	// actually changes (not on every no-op reconcile).
	for _, in := range inbounds {
		if !in.Enabled {
			continue
		}
		if kind, _, err := xray.ParseTarget(in.Target); err == nil && kind == xray.TargetDirect {
			s.log.Printf("inbound %q targets direct — its traffic egresses from this server's own IP", in.Name)
		}
	}

	switch {
	case !s.wd.IsStarted():
		s.log.Print("reconcile: starting xray")
		s.wd.Start()
	case s.running != nil && s.applyLive(cfg):
		// applied without a restart
	default:
		s.log.Print("reconcile: restarting xray with new config")
		s.restarts.Add(1)
		s.wd.Restart()
	}
	s.running = cfg
	s.loadedSlots = slots
	return nil
}

// applyLive tries to bring the running process from s.running to cfg without
// a restart, and reports whether it did. On false the caller restarts, which
// converges from whatever state a partial apply left behind.
func (s *Supervisor) applyLive(cfg []byte) bool {
	oldLC, err := parseLiveConfig(s.running)
	if err != nil {
		s.log.Printf("live apply: parse running config: %v — restarting", err)
		return false
	}
	newLC, err := parseLiveConfig(cfg)
	if err != nil {
		s.log.Printf("live apply: parse new config: %v — restarting", err)
		return false
	}
	plan := planLive(oldLC, newLC)
	if plan.restart {
		s.log.Printf("live apply: %s — restarting", plan.why)
		return false
	}
	if plan.empty() {
		return true
	}
	// A child started moments ago may not have bound its api yet.
	if err := s.waitAPIReady(5 * time.Second); err != nil {
		s.log.Printf("live apply: api not answering (%v) — restarting", err)
		return false
	}
	if err := s.applyLiveLocked(plan, cfg); err != nil {
		s.log.Printf("live apply failed: %v — restarting", err)
		return false
	}
	s.liveApplies.Add(1)
	s.log.Printf("applied live, no restart (outbounds -%d +%d, inbounds -%d +%d, routing %v)",
		len(plan.rmOut), len(plan.addOut), len(plan.rmIn), len(plan.addIn), plan.routing)
	return true
}

// SyncDialerMembers is Reconcile for callbacks with no error to return (a
// subscription refresh, a node or subscription toggle). Pool churn used to
// have its own ado/rmo delta path; the live apply now covers it — a member
// change is just outbounds added/removed plus a routing swap — so there is one
// path and one notion of what xray is running.
func (s *Supervisor) SyncDialerMembers() {
	if err := s.Reconcile(); err != nil {
		s.log.Printf("sync: %v", err)
	}
}

// Counters reports config restarts of a running xray (each dropped every
// connection) and changes applied live, since the daemon started.
func (s *Supervisor) Counters() (restarts, liveApplies int) {
	return int(s.restarts.Load()), int(s.liveApplies.Load())
}

// Stop terminates the xray child and its supervisor loop.
func (s *Supervisor) Stop() { s.wd.Stop(5 * time.Second) }

// RestartXray force-cycles the xray child: it stops the current process (and
// its supervise loop), regenerates config.json from current state, and starts
// fresh. Unlike Reconcile it never no-ops — this is the escape hatch for a
// wedged xray (balancer stuck, listeners not answering), so it must not depend
// on anything having changed. Returns the post-restart running state.
func (s *Supervisor) RestartXray() (bool, int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.healthMu.Lock()
	s.healthChecked = false
	s.healthOK = false
	s.healthFailures = 0
	s.healthMessage = "waiting for replacement xray health check"
	s.healthMu.Unlock()
	// Full stop first so a hung child is signalled (and killed after the grace
	// period) rather than merely asked to reload.
	s.wd.Stop(5 * time.Second)
	s.running, s.loadedSlots = nil, nil
	if err := s.reconcileLocked(); err != nil {
		return false, 0, err
	}
	if !s.wd.IsStarted() {
		return false, 0, nil // nothing routable — xray intentionally stays down
	}
	// The supervise loop spawns asynchronously; give it a moment to report a pid.
	deadline := time.Now().Add(5 * time.Second)
	for {
		running, pid, _, lastErr := s.wd.State()
		if running {
			return true, pid, nil
		}
		if time.Now().After(deadline) {
			if lastErr != "" {
				return false, 0, fmt.Errorf("%s", lastErr)
			}
			return false, 0, fmt.Errorf("xray did not come up within 5s")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// DownReason says why xray isn't running, given the watchdog's last error.
func (s *Supervisor) DownReason(lastErr string) string {
	if s.wd.IsStarted() {
		if lastErr == "" {
			return "starting"
		}
		return "restarting after: " + lastErr
	}
	if msg := s.ConfigError(); msg != "" {
		return msg + " (retrying every " + xrayHealthInterval.String() + ")"
	}
	if lastErr != "" {
		return lastErr
	}
	return "no enabled inbound with a port (add or enable one)"
}

// XrayState exposes child health for the Status RPC.
func (s *Supervisor) XrayState() (running bool, pid int, restarts int32, lastErr string) {
	return s.wd.State()
}

// StartHealthMonitor probes xray's local API. A live-but-wedged process is
// force-restarted after three consecutive failed checks; ordinary process
// exits remain the watchdog's responsibility.
func (s *Supervisor) StartHealthMonitor(ctx context.Context) {
	go func() {
		ticker := time.NewTicker(xrayHealthInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				s.checkXrayHealth()
			}
		}
	}()
}

func (s *Supervisor) checkXrayHealth() {
	running, _, _, _ := s.wd.State()
	if !running {
		// Down without the watchdog looping (startup reconcile refused, no
		// config yet): keep trying to bring it up. A no-op while nothing is
		// routable; a crash loop is the watchdog's own business.
		if !s.wd.IsStarted() {
			if err := s.Reconcile(); err != nil {
				s.log.Printf("xray is down, retry failed: %v", err)
			}
		}
		return
	}
	_, err := s.StatsQuery()
	if !s.recordHealthResult(err) {
		return
	}
	s.log.Printf("xray health check failed %d times — restarting", xrayHealthFailureLimit)
	_, _, restartErr := s.RestartXray()
	s.healthMu.Lock()
	s.healthFailures = 0
	s.healthRestarts++
	if restartErr != nil {
		s.healthOK = false
		s.healthMessage = "health restart failed: " + restartErr.Error()
	} else {
		s.healthChecked = false // the replacement child has not been probed yet
		s.healthOK = false
		s.healthMessage = "restarted after failed health checks"
	}
	s.healthMu.Unlock()
}

// recordHealthResult updates the health snapshot and reports whether the
// failure threshold was reached.
func (s *Supervisor) recordHealthResult(err error) bool {
	s.healthMu.Lock()
	defer s.healthMu.Unlock()
	s.healthChecked = true
	s.healthCheckedAt = time.Now()
	if err == nil {
		s.healthOK = true
		s.healthFailures = 0
		s.healthMessage = "ok"
		return false
	}
	s.healthOK = false
	s.healthFailures++
	s.healthMessage = fmt.Sprintf("API check failed (%d/%d): %v", s.healthFailures, xrayHealthFailureLimit, err)
	return s.healthFailures >= xrayHealthFailureLimit
}

// XrayHealth snapshots the responsiveness monitor for status output.
func (s *Supervisor) XrayHealth() (checked, responsive bool, message string, restarts int32, checkedAt time.Time) {
	s.healthMu.Lock()
	defer s.healthMu.Unlock()
	return s.healthChecked, s.healthOK, s.healthMessage, s.healthRestarts, s.healthCheckedAt
}

// resolveDialerSlots resolves each enabled master's Dialer refs into concrete
// pool members right now: xray: refs → the named entry's outbound; xraysub:
// refs → the subscription's ACTIVE nodes. Members dedupe by content Key.
// proxy: refs are not yet supported.
//
// Masters whose Dialer names the same refs (order-insensitive) share ONE slot
// (see xray.Slot): probe cost scales with unique pools, not with masters.
//
// Every enabled master gets a slot, INCLUDING one that resolves to zero members
// (sub not fetched yet, all nodes inactive, a bad ref). Dropping the slot would
// strip the master's dialerProxy and make it dial straight off this box —
// leaking the server's IP exactly when the pool is unavailable. An empty slot
// keeps the dialerProxy wired to a balancer that has nothing to pick, which
// falls back to `block`: the master fails closed until the pool fills. Past
// xray.SlotCount unique pools a master gets no slot, and Generate points its
// dialerProxy at the blackhole for the same reason.
func (s *Supervisor) resolveDialerSlots(entries []xray.XrayEntry) ([]xray.Slot, error) {
	byName := make(map[string]xray.XrayEntry, len(entries))
	var masters []xray.XrayEntry
	for _, e := range entries {
		byName[e.Name] = e
		if e.Enabled && e.IsMaster() {
			masters = append(masters, e)
		}
	}
	sort.Slice(masters, func(i, j int) bool { return masters[i].Name < masters[j].Name })

	var slots []xray.Slot
	slotByKey := map[string]int{} // dialer group key → index into slots
	parked := s.parker.parked()
	present := map[string]bool{} // every member key any pool resolves to
	defer func() { s.forgetRejectedExcept(present) }()
	for _, e := range masters {
		refs, err := xray.ParseDialer(e.Dialer)
		key := ""
		if err != nil {
			// Unparseable dialer still yields an empty slot, not no slot: the
			// master must stay behind the (empty → blocking) balancer rather
			// than fall back to dialing off this box. Its own key keeps it
			// from sharing a slot with anything.
			s.log.Printf("master %q: bad dialer: %v — pool empty, master blocked", e.Name, err)
			refs, key = nil, "invalid:"+e.Name
		} else {
			key = xray.DialerGroupKey(refs)
		}
		if i, ok := slotByKey[key]; ok {
			slots[i].Aliases = append(slots[i].Aliases, e.Name)
			continue
		}
		resolved := s.resolveDialerMembers(refs, byName)
		for _, m := range resolved {
			present[m.Key] = true
		}
		members := withoutParked(s.withoutRejected(resolved), parked)
		if len(members) == 0 {
			s.log.Printf("master %q: dialer resolved to 0 members — master blocked until pool fills", e.Name)
		}
		slotByKey[key] = len(slots)
		slots = append(slots, xray.Slot{Master: e.Name, Key: key, Index: -1, Members: members})
	}
	return s.assignSlotIndices(slots), nil
}

// assignSlotIndices gives every pool a slot index, keeping the one it had on
// the previous reconcile when possible (a changed index renames every tag and
// port of the slot, which the live apply would have to replace), and the
// lowest free index otherwise. Pools past SlotCount get none and are dropped;
// Generate then blocks their masters. Returned sorted by index.
func (s *Supervisor) assignSlotIndices(slots []xray.Slot) []xray.Slot {
	if s.slotIdx == nil {
		s.slotIdx = map[string]int{}
	}
	used := map[int]bool{}
	for i := range slots {
		if idx, ok := s.slotIdx[slots[i].Key]; ok && idx < xray.SlotCount && !used[idx] {
			slots[i].Index = idx
			used[idx] = true
		}
	}
	next := 0
	var out []xray.Slot
	for _, sl := range slots {
		if sl.Index < 0 {
			for next < xray.SlotCount && used[next] {
				next++
			}
			if next >= xray.SlotCount {
				s.log.Printf("master %q: all %d dialer slots in use — master blocked", sl.Master, xray.SlotCount)
				continue
			}
			sl.Index = next
			used[next] = true
		}
		out = append(out, sl)
	}
	s.slotIdx = map[string]int{}
	for _, sl := range out {
		s.slotIdx[sl.Key] = sl.Index
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Index < out[j].Index })
	return out
}

// resolveDialerMembers expands dialer refs into member outbounds, deduped by
// stable key.
func (s *Supervisor) resolveDialerMembers(refs []xray.DialerRef, byName map[string]xray.XrayEntry) []xray.SlotMember {
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
				if err := xray.CheckMemberOutbound(m.Outbound); err != nil {
					s.log.Printf("dialer member %q skipped: %v", r.Name, err)
					continue
				}
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
				if xray.CheckMemberOutbound(n.Outbound) != nil {
					continue // the link parser only yields proxies; defensive
				}
				add(n.Fingerprint, n.Outbound)
			}
		case xray.RefProxy:
			// proxy upstreams not yet modelled — skipped.
		}
	}
	return members
}

// routable reports whether xray has anything to serve: any enabled inbound with
// a TCP port or Unix socket.
func routable(inbounds []xray.Inbound) bool {
	for _, in := range inbounds {
		if in.Enabled && (in.Port != 0 || xray.IsUnixInbound(in)) {
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
		clearStaleSockets(p.XrayConfig(), logger, func() []int { return ReapMainXray(p, logger) })
		cmd := exec.Command(bin, "run", "-c", p.XrayConfig())
		dieWithParent(cmd) // never outlive the daemon and keep holding the ports
		cmd.Env = append(os.Environ(), "XRAY_LOCATION_ASSET="+p.AssetDir())
		// xray writes its own access/error logs (paths baked into config); its
		// stdout/stderr go to the daemon error log for startup diagnostics.
		// The tail also rides along so a failed start says why in Status.
		out := &tailWriter{}
		if errLog, err := os.OpenFile(p.ErrorLog(), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644); err == nil {
			out.w = errLog
		}
		cmd.Stdout = out
		cmd.Stderr = out
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
