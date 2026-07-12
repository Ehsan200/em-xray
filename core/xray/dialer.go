package xray

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// Observatory probe defaults.
const (
	DefaultProbeURL           = "http://www.gstatic.com/generate_204"
	DefaultProbeInterval      = "60s"
	ObservatorySelectorPrefix = "slot"
)

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

// Slot is a master's resolved pool: the master entry name + its members.
type Slot struct {
	Master  string
	Members []SlotMember
}

// SlotIndexByName maps each master to its slot index (sorted-name order), the
// SAME ordering Generate uses. The live-sync path relies on this to address the
// correct slotN-out- tags without regenerating config.
func SlotIndexByName(slots []Slot) map[string]int {
	s := append([]Slot(nil), slots...)
	sort.Slice(s, func(i, j int) bool { return s[i].Master < s[j].Master })
	m := make(map[string]int, len(s))
	for i, x := range s {
		m[x.Master] = i
	}
	return m
}

// Slot tag/port helpers. Index is the master's position in sorted-name order.
func SlotInTag(idx int) string      { return fmt.Sprintf("slot%d-in", idx) }
func SlotBalTag(idx int) string     { return fmt.Sprintf("slot%d-bal", idx) }
func SlotOutPrefix(idx int) string  { return fmt.Sprintf("slot%d-out-", idx) }
func SlotPort(idx int) int          { return SlotPortStart + idx }
func DialerTag(master string) string { return "dialer-" + sanitizeKey(master) }

// SlotMemberTag is the tag of a member outbound within a slot. The shared
// prefix (slotN-out-) is what lets the balancer + observatory adopt live-added
// members with no config reload.
func SlotMemberTag(idx int, key string) string {
	return SlotOutPrefix(idx) + sanitizeKey(key)
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
	return ob, nil
}
