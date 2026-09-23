package xray

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// Burst-observatory probe defaults. The burst observatory health-pings each
// pool member every interval and keeps a rolling window of `sampling` results;
// leastLoad demotes a node on a failed ping instead of waiting out the classic
// observatory's averaged 60s probe, so a dying node is switched away from
// within one or two pings.
//
// 10s keeps the cost low (each ping is a full outbound dial through the node,
// and probe fan-out already scales with unique pools, not masters). Three
// samples with a 5s timeout ride out one slow or retransmitted ping on a lossy
// uplink; a node that is really dead still fails every one.
const (
	DefaultProbeURL           = "http://www.gstatic.com/generate_204"
	DefaultProbeInterval      = "10s"
	DefaultProbeSampling      = 3
	DefaultPingTimeout        = "5s"
	ObservatorySelectorPrefix = "slot"
)

// SlotBalancerExpected is how many best-ranked members a slot's leastLoad
// balancer spreads connections over. Two, not one: with a single winner every
// connection of every master on the pool rides one node, and that node dying
// drops all of them at once. It costs nothing in exit identity — a master's
// exit IP is its own server whichever node carries the tunnel.
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
// (first by name), Aliases the rest. They share the slot inbound, balancer and
// member outbounds instead of getting a copy each — every copy is a full set of
// outbounds the observatory probes on its own, so N masters on one
// subscription used to mean N probes per node per interval. Each master still
// keeps its own dialer-NAME outbound, so sharing never changes a master's own
// outbound.
type Slot struct {
	Master  string
	Aliases []string
	Key     string // DialerGroupKey of the refs the slot serves
	Index   int
	Members []SlotMember
}

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
func SlotBalTag(idx int) string      { return fmt.Sprintf("slot%d-bal", idx) }
func SlotOutPrefix(idx int) string   { return fmt.Sprintf("slot%d-out-", idx) }
func SlotPort(idx int) int           { return SlotPortStart + idx }
func DialerTag(master string) string { return "dialer-" + sanitizeKey(master) }

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
	return ob, nil
}
