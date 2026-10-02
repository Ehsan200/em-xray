package xray

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"net"
	"sort"
	"strings"
)

// Burst-observatory probe defaults. xray pings each pool member `sampling`
// times per round of interval×sampling (at random moments inside the round)
// and only records the results when the round ends; a member counts as alive
// while any ping of that last round succeeded. So a member that dies is seen
// dead at the end of the round it died in — up to interval×sampling plus the
// ping timeout later — and the balancer skips it from then on.
//
// The per-member ping rate is one per interval whatever the sampling, so two
// samples, not three, shortens that detection by a third (20s instead of 30s
// at the default 10s interval) at no extra probe cost. Two still ride out one
// lost ping on a lossy uplink; a node that is really dead fails both. 10s
// keeps the cost low: each ping is a full outbound dial through the node, and
// probe fan-out already scales with unique pools, not masters.
//
// The ping is HTTPS: a node can relay plain HTTP fine and still break TLS
// through it (seen on real subscriptions), and every master connection it
// carries is TLS.
const (
	DefaultProbeURL           = "https://www.gstatic.com/generate_204"
	DefaultProbeInterval      = "10s"
	DefaultProbeSampling      = 2
	DefaultPingTimeout        = "5s"
	ObservatorySelectorPrefix = "slot"
)

// SlotBalancerExpected is how many best-ranked members a slot's balancer
// spreads connections over (the daemon's active pick, or leastLoad's expected
// before the daemon has ranked the pool). Two, not one: with a single winner
// every connection of every master on the pool rides one node, and that node
// dying drops all of them at once. It costs nothing in exit identity — a
// master's exit IP is its own server whichever node carries the tunnel.
const SlotBalancerExpected = 2

// Dialer ref kinds.
const (
	RefXray    = "xray"    // one user xray entry
	RefXraySub = "xraysub" // all active nodes of a subscription
	RefProxy   = "proxy"   // a user proxy upstream (synth outbound)
)

// DialerRef is one typed reference inside an entry's Dialer field.
type DialerRef struct {
	Kind string // xray | xraysub | proxy
	Name string
}

// ParseDialer parses a comma-separated Dialer field into typed refs. An empty
// string yields no refs (the entry is not a master).
func ParseDialer(s string) ([]DialerRef, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, nil
	}
	var refs []DialerRef
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		kind, name, ok := strings.Cut(part, ":")
		if !ok || strings.TrimSpace(name) == "" {
			return nil, fmt.Errorf("bad dialer ref %q (want kind:NAME)", part)
		}
		switch kind {
		case RefXray, RefXraySub, RefProxy:
		default:
			return nil, fmt.Errorf("unknown dialer kind %q in %q", kind, part)
		}
		refs = append(refs, DialerRef{Kind: kind, Name: strings.TrimSpace(name)})
	}
	return refs, nil
}

// DetectDialerCycle reports an error if making entry `name` a master with the
// given dialer would route its transport through itself, directly or
// transitively. Only xray: refs can form a loop (xraysub:/proxy: are leaves).
// `dialerOf` returns the stored Dialer string for an entry name (the proposed
// row's own dialer is supplied via `name`/`dialer`, treated as if stored).
func DetectDialerCycle(name, dialer string, dialerOf func(entry string) (string, bool)) error {
	name = NormalizeName(name)
	lookup := func(entry string) (string, bool) {
		if NormalizeName(entry) == name {
			return dialer, true
		}
		return dialerOf(entry)
	}
	visiting := map[string]bool{}
	var walk func(entry string) error
	walk = func(entry string) error {
		entry = NormalizeName(entry)
		if visiting[entry] {
			return fmt.Errorf("dialer cycle through %q", entry)
		}
		visiting[entry] = true
		defer delete(visiting, entry)

		d, ok := lookup(entry)
		if !ok {
			return nil // referenced entry not found here → leaf for cycle purposes
		}
		refs, err := ParseDialer(d)
		if err != nil {
			return err
		}
		for _, r := range refs {
			if r.Kind != RefXray {
				continue // only xray refs can loop
			}
			if NormalizeName(r.Name) == name {
				return fmt.Errorf("dialer cycle: %q routes through itself", name)
			}
			if err := walk(r.Name); err != nil {
				return err
			}
		}
		return nil
	}
	return walk(name)
}

// RenameDialerRef rewrites refs of the given kind from oldName to newName in a
// Dialer string, deduping any collision, and reports whether anything changed.
// Used by the rename cascade so a master keeps pointing at a renamed entry/sub.
func RenameDialerRef(dialer, kind, oldName, newName string) (string, bool) {
	refs, err := ParseDialer(dialer)
	if err != nil {
		return dialer, false
	}
	changed := false
	seen := map[string]bool{}
	var out []string
	for _, r := range refs {
		if r.Kind == kind && NormalizeName(r.Name) == NormalizeName(oldName) {
			r.Name = newName
			changed = true
		}
		key := r.Kind + ":" + r.Name
		if seen[key] {
			changed = true // a collision merged two refs into one
			continue
		}
		seen[key] = true
		out = append(out, key)
	}
	return strings.Join(out, ","), changed
}

// SlotMember is one resolved dialer pool member: a stable Key (content identity)
// and the raw outbound JSON to dial through.
type SlotMember struct {
	Key      string // node fingerprint | xray-NAME | proxy-NAME
	Outbound string // raw outbound JSON
}

// Slot is one resolved node pool: its index (→ port + tags), the masters wired
// to it, and the members its balancer selects among.
//
// Masters whose Dialer names the same refs share one slot: Master is the owner
// (first by name), Aliases the rest. They share the slot inbound and member
// outbounds instead of getting a copy each — every copy is a full set of
// outbounds the observatory probes on its own, so N masters on one
// subscription used to mean N probes per node per interval. Each master still
// keeps its own dialer-NAME outbound and its own balancer: its dialer hop logs
// into the slot as its own user, and routing sends each user to its master's
// balancer, because a node that carries one master can fail another.
type Slot struct {
	Master  string
	Aliases []string
	Key     string // DialerGroupKey of the refs the slot serves
	Index   int
	Members []SlotMember
	// Picked is the daemon's ranking: the first ActiveCount() keys are the
	// balancer's selector, the next is its fallback (a hot spare). Empty =
	// not ranked yet, leastLoad over every member.
	Picked []string
	// Active is how many of Picked are active; 0 = SlotBalancerExpected.
	Active int
	// MasterPicks overrides Picked/Active for one master (owner or alias):
	// its own ranking, from probing that master through each member. A node
	// can carry one master and not another, so masters sharing a pool each
	// get their own balancer. A master absent here uses Picked/Active.
	MasterPicks map[string]MasterPick
	// Strategy is the pool's effective switch strategy: StrategyStable or
	// StrategyAgile ("" = stable).
	Strategy string
	// Auto: Strategy was chosen by the daemon's auto classifier, not set.
	Auto bool
	// Pinned are a manual pool's pinned member keys (present in Members).
	Pinned []string
}

// MasterPick is one master's ranking within its slot, as Slot.Picked/Active.
type MasterPick struct {
	Picked []string
	Active int
}

// ActiveCount is how many leading Picked keys the balancer spreads over.
func (p MasterPick) ActiveCount() int { return activeCount(p.Active, p.Picked) }

// ActiveCount is how many leading Picked keys the balancer spreads over.
func (s Slot) ActiveCount() int { return activeCount(s.Active, s.Picked) }

func activeCount(active int, picked []string) int {
	if active <= 0 {
		active = SlotBalancerExpected
	}
	return min(active, len(picked))
}

// PickOf is master's ranking: its own when it has one, else the pool's.
func (s Slot) PickOf(master string) MasterPick {
	if p, ok := s.MasterPicks[master]; ok {
		return p
	}
	return MasterPick{Picked: s.Picked, Active: s.Active}
}

// ActiveKeys is every member some master of the slot routes through: the
// union of the masters' active picks.
func (s Slot) ActiveKeys() map[string]bool {
	out := map[string]bool{}
	for _, m := range s.SlotMasters() {
		p := s.PickOf(m)
		for _, k := range p.Picked[:p.ActiveCount()] {
			out[k] = true
		}
	}
	return out
}

// PickedKeys is every member in some master's pick, active or spare.
func (s Slot) PickedKeys() map[string]bool {
	out := map[string]bool{}
	for _, m := range s.SlotMasters() {
		for _, k := range s.PickOf(m).Picked {
			out[k] = true
		}
	}
	return out
}

// Ranked reports whether the daemon has ranked the pool yet.
func (s Slot) Ranked() bool { return len(s.Picked) > 0 || len(s.MasterPicks) > 0 }

// Agile reports whether the pool runs the agile strategy.
func (s Slot) Agile() bool { return s.Strategy == StrategyAgile }

// Manual reports whether the pool routes only through pinned members.
func (s Slot) Manual() bool { return s.Strategy == StrategyManual }

// NeverParks reports whether the pool keeps every member loaded: agile pools
// follow nodes that come back, manual ones route only where told.
func (s Slot) NeverParks() bool { return s.Agile() || s.Manual() }

// SlotMasters returns every master wired to the slot, owner first.
func (s Slot) SlotMasters() []string {
	return append([]string{s.Master}, s.Aliases...)
}

// SlotIndexByName maps every master (owner and alias) to its slot index.
func SlotIndexByName(slots []Slot) map[string]int {
	m := make(map[string]int, len(slots))
	for _, s := range slots {
		for _, name := range s.SlotMasters() {
			m[name] = s.Index
		}
	}
	return m
}

// DialerGroupKey identifies a dialer by the set of refs it names, ignoring
// order and duplicates: the balancer picks by health, not position, so two
// masters listing the same refs differently are served by one slot.
func DialerGroupKey(refs []DialerRef) string {
	seen := map[string]bool{}
	var parts []string
	for _, r := range refs {
		k := r.Kind + ":" + r.Name
		if !seen[k] {
			seen[k] = true
			parts = append(parts, k)
		}
	}
	sort.Strings(parts)
	return strings.Join(parts, ",")
}

// Slot tag/port helpers. idx is Slot.Index.
func SlotInTag(idx int) string       { return fmt.Sprintf("slot%d-in", idx) }
func SlotOutPrefix(idx int) string   { return fmt.Sprintf("slot%d-out-", idx) }
func SlotPort(idx int) int           { return SlotPortStart + idx }
func DialerTag(master string) string { return "dialer-" + sanitizeKey(master) }

// SlotBalTag is master's balancer in slot idx: every master on a slot gets
// its own, so each can be steered to the nodes that carry it.
func SlotBalTag(idx int, master string) string {
	return fmt.Sprintf("slot%d-bal-%s", idx, sanitizeKey(master))
}

// DialerEmail is the user a master's dialer hop logs into its slot as; the
// slot's routing sends each user to that master's balancer.
func DialerEmail(master string) string { return "dialer-" + sanitizeKey(master) }

// DialerUUID is the vless id of master's dialer hop, derived from the name.
// The slot listens on loopback only, so it identifies, it doesn't protect.
func DialerUUID(master string) string {
	h := sha256.Sum256([]byte("emx-dialer:" + master))
	h[6] = h[6]&0x0f | 0x40
	h[8] = h[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", h[0:4], h[4:6], h[6:8], h[8:10], h[10:16])
}

// DialerSourceAddr is the loopback address a master's dialer hop connects
// from (GenOptions.DialerSource). Every connection the master makes into its
// slot carries it, so when the master's dialer changes the daemon can close
// exactly that master's old connections (daemon/sockcut_linux.go) and the
// apps behind them reconnect onto the new path, while every other master's
// connections are left alone. It lies in 127.64.0.0/10, so never 127.0.0.1 or
// a resolver stub address, and is derived from the name, so it survives
// restarts and slot moves. Two masters colliding on its 22 bits would only
// share a cut.
func DialerSourceAddr(master string) string {
	h := fnv.New32a()
	_, _ = h.Write([]byte(master))
	v := h.Sum32()
	return fmt.Sprintf("127.%d.%d.%d", 64+((v>>16)&63), (v>>8)&255, v&255)
}

// SlotMemberTag is the tag of a member outbound within a slot. The shared
// prefix (slotN-out-) is what lets the balancer + observatory adopt live-added
// members with no config reload.
func SlotMemberTag(idx int, key string) string {
	return SlotOutPrefix(idx) + sanitizeKey(key)
}

// tunnelProtocols are the outbound protocols that carry a connection to a
// remote proxy. A pool member must be one of them: a freedom (or dns,
// loopback…) member would dial the master's server straight off this box —
// exactly what the dialerProxy hop exists to prevent — and a blackhole member
// is just dead weight the balancer might pick.
var tunnelProtocols = map[string]bool{
	"vless": true, "vmess": true, "trojan": true, "shadowsocks": true,
	"socks": true, "http": true, "hysteria": true, "wireguard": true,
}

// CheckMasterOutbound reports why an outbound can't be a master (an entry with
// a dialer), or nil. xray's hysteria client dials its QUIC socket itself and
// ignores sockopt.dialerProxy, so a hysteria master would skip its pool and
// reach its server straight off this box — silently, when that path exists.
func CheckMasterOutbound(outbound string) error {
	var ob struct {
		Protocol string `json:"protocol"`
	}
	if err := json.Unmarshal([]byte(outbound), &ob); err != nil {
		return fmt.Errorf("bad outbound json: %w", err)
	}
	if strings.EqualFold(ob.Protocol, "hysteria") {
		return fmt.Errorf("hysteria ignores dialer chains in xray (it would dial its server directly); use a vless/vmess/trojan master behind the pool instead")
	}
	return nil
}

// CheckMemberOutbound reports why an outbound can't be a pool member, or nil.
func CheckMemberOutbound(outbound string) error {
	var ob struct {
		Protocol string `json:"protocol"`
	}
	if err := json.Unmarshal([]byte(outbound), &ob); err != nil {
		return fmt.Errorf("bad outbound json: %w", err)
	}
	if !tunnelProtocols[strings.ToLower(ob.Protocol)] {
		return fmt.Errorf("protocol %q does not tunnel to a remote proxy (a pool member must never egress from this box)", ob.Protocol)
	}
	return nil
}

// MemberOutboundJSON produces the member outbound object for a slot member,
// tagged and healed. It MUST be byte-for-byte identical to what a live
// `xray api ado` adds (P6), so both baked and live members share exact form.
func MemberOutboundJSON(idx int, m SlotMember) (map[string]any, error) {
	var ob map[string]any
	if err := json.Unmarshal([]byte(m.Outbound), &ob); err != nil {
		return nil, fmt.Errorf("member %q: bad outbound json: %w", m.Key, err)
	}
	ob["tag"] = SlotMemberTag(idx, m.Key)
	healXHTTPExtra(ob)
	healAllowInsecure(ob)
	setStallTimeouts(ob)
	return ob, nil
}

// Stall timeouts for member sockets: a black-holed node otherwise hangs its
// connections until the kernel gives up (minutes) instead of failing over.
const (
	memberTCPUserTimeoutMs  = 15000
	memberTCPKeepAliveIdle  = 30
	memberTCPKeepAliveIntvl = 10
)

// setStallTimeouts sets them unless the node's outbound already does.
func setStallTimeouts(ob map[string]any) {
	ss, ok := ob["streamSettings"].(map[string]any)
	if !ok {
		ss = map[string]any{}
		ob["streamSettings"] = ss
	}
	so, ok := ss["sockopt"].(map[string]any)
	if !ok {
		so = map[string]any{}
		ss["sockopt"] = so
	}
	for k, v := range map[string]any{
		"tcpUserTimeout":       memberTCPUserTimeoutMs,
		"tcpKeepAliveIdle":     memberTCPKeepAliveIdle,
		"tcpKeepAliveInterval": memberTCPKeepAliveIntvl,
	} {
		if _, set := so[k]; !set {
			so[k] = v
		}
	}
}

// OutboundEndpoint returns the server host and port a proxy outbound dials:
// settings.vnext / settings.servers (first entry), the flat settings form,
// or a wireguard peer's endpoint. ok is false when none is found.
func OutboundEndpoint(outbound string) (host string, port int, ok bool) {
	var ob struct {
		Settings map[string]any `json:"settings"`
	}
	if json.Unmarshal([]byte(outbound), &ob) != nil || ob.Settings == nil {
		return "", 0, false
	}
	hostPort := func(m map[string]any) (string, int, bool) {
		h, _ := m["address"].(string)
		var p int
		switch v := m["port"].(type) {
		case float64:
			p = int(v)
		case string:
			_, _ = fmt.Sscanf(v, "%d", &p)
		}
		return h, p, h != "" && p > 0
	}
	for _, list := range []string{"vnext", "servers"} {
		if arr, _ := ob.Settings[list].([]any); len(arr) > 0 {
			if m, _ := arr[0].(map[string]any); m != nil {
				return hostPort(m)
			}
		}
	}
	if peers, _ := ob.Settings["peers"].([]any); len(peers) > 0 {
		if m, _ := peers[0].(map[string]any); m != nil {
			ep, _ := m["endpoint"].(string)
			h, p, err := net.SplitHostPort(ep)
			if err != nil {
				return "", 0, false
			}
			var n int
			_, _ = fmt.Sscanf(p, "%d", &n)
			return h, n, h != "" && n > 0
		}
	}
	return hostPort(ob.Settings)
}
