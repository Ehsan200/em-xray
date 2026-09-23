package daemon

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"time"
)

// Live config application.
//
// Restarting xray drops every client connection through every inbound and
// resets the observatory, so a config change reaches the running process as
// the smallest set of API calls that reproduces it: inbounds and outbounds are
// replaced by tag (adi/rmi/ado/rmo; a changed object is removed and re-added),
// and the routing section — rules AND balancers — is swapped whole (`adrules`
// without -append replaces it). Connections through anything that did not
// change are not touched.
//
// Only a change outside those three sections (log, api, stats, policy,
// burstObservatory, metrics) or to the FIRST outbound — xray's default for
// unmatched traffic, which the API can't reorder — needs a restart. Those are
// rare and deliberate (a log level or probe interval change, an upgrade).
// Invariants that keep everything else live: the api inbound and the burst
// observatory are always emitted, and `block` is always outbounds[0].

// liveConfig is a generated xray config decomposed into the units the API can
// replace independently. Every value is canonical JSON (keys sorted), so
// equal configs compare equal regardless of formatting.
type liveConfig struct {
	fixed     string                     // every top-level key except inbounds/outbounds/routing
	inbounds  map[string]string          // tag → object
	outbounds map[string]string          // tag → object
	dialerOf  map[string]string          // outbound tag → its sockopt.dialerProxy ("" = none)
	creds     map[string]map[string]bool // inbound tag → client credentials it accepts
	firstOut  string                     // tag of the first outbound
	routing   string
}

func parseLiveConfig(raw []byte) (liveConfig, error) {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(raw, &top); err != nil {
		return liveConfig{}, err
	}
	lc := liveConfig{inbounds: map[string]string{}, outbounds: map[string]string{}, dialerOf: map[string]string{}, creds: map[string]map[string]bool{}}
	fixed := map[string]json.RawMessage{}
	for k, v := range top {
		switch k {
		case "inbounds", "outbounds", "routing":
		default:
			fixed[k] = v
		}
	}
	var err error
	if lc.fixed, err = canonicalJSON(fixed); err != nil {
		return liveConfig{}, err
	}
	if lc.routing, err = canonicalJSON(top["routing"]); err != nil {
		return liveConfig{}, err
	}
	if _, err = taggedObjects(top["inbounds"], lc.inbounds, nil); err != nil {
		return liveConfig{}, fmt.Errorf("inbounds: %w", err)
	}
	for tag, obj := range lc.inbounds {
		lc.creds[tag] = inboundCredentials(obj)
	}
	order, err := taggedObjects(top["outbounds"], lc.outbounds, lc.dialerOf)
	if err != nil {
		return liveConfig{}, fmt.Errorf("outbounds: %w", err)
	}
	if len(order) > 0 {
		lc.firstOut = order[0]
	}
	return lc, nil
}

// taggedObjects fills into with tag → canonical object (and dialerOf, when
// non-nil, with tag → dialerProxy) and returns the tags in array order.
// Untagged or duplicate-tagged objects can't be addressed through the API.
func taggedObjects(raw json.RawMessage, into, dialerOf map[string]string) ([]string, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil, err
	}
	order := make([]string, 0, len(items))
	for _, it := range items {
		var t struct {
			Tag            string `json:"tag"`
			StreamSettings struct {
				Sockopt struct {
					DialerProxy string `json:"dialerProxy"`
				} `json:"sockopt"`
			} `json:"streamSettings"`
		}
		if err := json.Unmarshal(it, &t); err != nil {
			return nil, err
		}
		if t.Tag == "" {
			return nil, fmt.Errorf("object without a tag")
		}
		if _, dup := into[t.Tag]; dup {
			return nil, fmt.Errorf("duplicate tag %q", t.Tag)
		}
		c, err := canonicalJSON(it)
		if err != nil {
			return nil, err
		}
		into[t.Tag] = c
		if dialerOf != nil {
			dialerOf[t.Tag] = t.StreamSettings.Sockopt.DialerProxy
		}
		order = append(order, t.Tag)
	}
	return order, nil
}

// canonicalJSON re-encodes v with object keys sorted (encoding/json sorts map
// keys), so formatting differences never read as a change.
func canonicalJSON(v any) (string, error) {
	if raw, ok := v.(json.RawMessage); ok {
		if len(raw) == 0 {
			return "", nil
		}
		var anyV any
		if err := json.Unmarshal(raw, &anyV); err != nil {
			return "", err
		}
		v = anyV
	}
	b, err := json.Marshal(v)
	return string(b), err
}

// inboundCredentials returns the client credentials an inbound accepts: vless /
// vmess ids, trojan passwords, hysteria auth strings, socks accounts.
func inboundCredentials(obj string) map[string]bool {
	var ib struct {
		Settings struct {
			Clients []struct {
				ID       string `json:"id"`
				Password string `json:"password"`
				Auth     string `json:"auth"` // hysteria
			} `json:"clients"`
			Accounts []struct {
				User string `json:"user"`
				Pass string `json:"pass"`
			} `json:"accounts"`
		} `json:"settings"`
	}
	out := map[string]bool{}
	if json.Unmarshal([]byte(obj), &ib) != nil {
		return out
	}
	for _, c := range ib.Settings.Clients {
		out["id:"+c.ID+"|pw:"+c.Password+"|auth:"+c.Auth] = true
	}
	for _, a := range ib.Settings.Accounts {
		out["socks:"+a.User+":"+a.Pass] = true
	}
	return out
}

// livePlan is the API work that turns one config into another.
type livePlan struct {
	restart bool   // a change the API can't express
	why     string // why restart

	rmOut, addOut []string // tags; a changed outbound is in both
	rmIn, addIn   []string
	routing       bool
	dialerOf      map[string]string // outbound tag → dialerProxy, old ∪ new
}

func (p livePlan) empty() bool {
	return !p.restart && !p.routing && len(p.rmOut)+len(p.addOut)+len(p.rmIn)+len(p.addIn) == 0
}

func planLive(old, want liveConfig) livePlan {
	switch {
	case old.fixed != want.fixed:
		return livePlan{restart: true, why: "a section the api can't change (log, api, stats, policy, observatory, metrics) differs"}
	case old.firstOut != want.firstOut:
		return livePlan{restart: true, why: "the first (default) outbound changed"}
	}
	// Revocation must cut live sessions. xray keeps every connection an
	// inbound already accepted when that inbound is removed or replaced, so a
	// disabled, deleted, over-quota or re-keyed client would stay connected
	// after a live apply. Only a restart ends them.
	for tag, old := range old.creds {
		for c := range old {
			if !want.creds[tag][c] {
				return livePlan{restart: true, why: "a client credential of " + tag + " was revoked (live sessions only end on restart)"}
			}
		}
	}
	p := livePlan{dialerOf: map[string]string{}}
	for tag, d := range old.dialerOf {
		p.dialerOf[tag] = d
	}
	for tag, d := range want.dialerOf {
		if d != "" {
			p.dialerOf[tag] = d
		}
	}
	p.rmOut, p.addOut = diffTagged(old.outbounds, want.outbounds)

	// A master whose dialer outbound is replaced or removed is taken down
	// FIRST and brought back LAST, so it never exists while its dialer-NAME
	// doesn't. xray v26.3.27 refuses a dial whose dialerProxy names a missing
	// outbound (verified against the binary), but whether that falls back to
	// a direct dial is an xray implementation detail the no-leak guarantee
	// should not rest on. With the master itself missing, its routing rule
	// points at nothing and xray sends the connection to outbounds[0], the
	// blackhole: fail closed for the few milliseconds of the swap.
	gone := toSet(p.rmOut)
	for tag, d := range want.dialerOf {
		if d == "" || !gone[d] || gone[tag] {
			continue
		}
		if _, existed := old.outbounds[tag]; existed {
			p.rmOut = append(p.rmOut, tag)
			p.addOut = append(p.addOut, tag)
		}
	}
	sort.Strings(p.rmOut)
	sort.Strings(p.addOut)

	p.rmIn, p.addIn = diffTagged(old.inbounds, want.inbounds)
	p.routing = old.routing != want.routing
	return p
}

// diffTagged returns the tags to remove (gone or changed) and to add (new or
// changed), each sorted for deterministic API calls.
func diffTagged(old, want map[string]string) (rm, add []string) {
	for tag, o := range old {
		if w, ok := want[tag]; !ok || w != o {
			rm = append(rm, tag)
		}
	}
	for tag, w := range want {
		if o, ok := old[tag]; !ok || w != o {
			add = append(add, tag)
		}
	}
	sort.Strings(rm)
	sort.Strings(add)
	return rm, add
}

// dependentsFirst orders tags so outbounds that dial through another outbound
// (dialerProxy set) come before the plain ones: the order removals must run.
// Additions run in the reverse order.
func (p livePlan) dependentsFirst(tags []string) []string {
	out := append([]string(nil), tags...)
	sort.SliceStable(out, func(i, j int) bool {
		return p.dialerOf[out[i]] != "" && p.dialerOf[out[j]] == ""
	})
	return out
}

func reversed(xs []string) []string {
	out := make([]string, len(xs))
	for i, x := range xs {
		out[len(xs)-1-i] = x
	}
	return out
}

// applyLiveLocked moves the running process to want through the API. Any
// failure returns an error and the caller restarts, which converges from any
// intermediate state (the new config is already on disk). Caller holds s.mu.
//
// Order keeps every reference resolvable: changed outbounds are removed
// (masters before their dialers), outbounds added (dialers before their
// masters), then inbounds, then routing, and only then the removal of things
// nothing references any more.
func (s *Supervisor) applyLiveLocked(p livePlan, want []byte) error {
	var top struct {
		Inbounds  []json.RawMessage `json:"inbounds"`
		Outbounds []json.RawMessage `json:"outbounds"`
		Routing   json.RawMessage   `json:"routing"`
	}
	if err := json.Unmarshal(want, &top); err != nil {
		return err
	}
	addOut := toSet(p.addOut)
	addIn := toSet(p.addIn)

	if changed := intersect(p.rmOut, addOut); len(changed) > 0 {
		if err := s.apiRun("rmo", p.dependentsFirst(changed)...); err != nil {
			return err
		}
	}
	for _, tag := range reversed(p.dependentsFirst(p.addOut)) {
		// One outbound per call so a master is never added in the same
		// request as — and possibly before — its dialer.
		if err := s.apiWithDoc("ado", map[string]any{"outbounds": pick(top.Outbounds, []string{tag})}); err != nil {
			return err
		}
	}
	if changed := intersect(p.rmIn, addIn); len(changed) > 0 {
		if err := s.apiRun("rmi", changed...); err != nil {
			return err
		}
	}
	if len(p.addIn) > 0 {
		if err := s.apiWithDoc("adi", map[string]any{"inbounds": pick(top.Inbounds, p.addIn)}); err != nil {
			return err
		}
	}
	if p.routing {
		// No -append: replaces the rules AND balancers wholesale.
		if err := s.apiWithDoc("adrules", map[string]any{"routing": top.Routing}); err != nil {
			return err
		}
	}
	if gone := minus(p.rmIn, addIn); len(gone) > 0 {
		if err := s.apiRun("rmi", gone...); err != nil {
			return err
		}
	}
	if gone := minus(p.rmOut, addOut); len(gone) > 0 {
		if err := s.apiRun("rmo", p.dependentsFirst(gone)...); err != nil {
			return err
		}
	}
	return nil
}

// pick returns the objects in items whose tag is in tags, in tags order.
func pick(items []json.RawMessage, tags []string) []json.RawMessage {
	byTag := make(map[string]json.RawMessage, len(items))
	for _, it := range items {
		var t struct {
			Tag string `json:"tag"`
		}
		if json.Unmarshal(it, &t) == nil {
			byTag[t.Tag] = it
		}
	}
	var out []json.RawMessage
	for _, tag := range tags {
		if it, ok := byTag[tag]; ok {
			out = append(out, it)
		}
	}
	return out
}

// apiRun runs `xray api <sub> args...` against the running child.
func (s *Supervisor) apiRun(sub string, args ...string) error {
	cmd, err := s.apiCmd(sub, args...)
	if err != nil {
		return err
	}
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("api %s: %v: %s", sub, err, out)
	}
	return nil
}

// apiWithDoc writes doc to a temp file and hands it to `xray api <sub>`,
// which only accepts config files.
func (s *Supervisor) apiWithDoc(sub string, doc any) error {
	b, err := json.Marshal(doc)
	if err != nil {
		return err
	}
	dir := s.paths.AdoDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, "api-"+sub+"-*.json")
	if err != nil {
		return err
	}
	path := f.Name()
	defer os.Remove(path)
	if _, err := f.Write(b); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return s.apiRun(sub, path)
}

// waitAPIReady polls the api (a cheap statsquery) until it answers or the
// timeout passes. A process started moments ago has not bound it yet.
func (s *Supervisor) waitAPIReady(timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		_, err := s.StatsQuery()
		if err == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return err
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func toSet(xs []string) map[string]bool {
	m := make(map[string]bool, len(xs))
	for _, x := range xs {
		m[x] = true
	}
	return m
}

func intersect(xs []string, set map[string]bool) []string {
	var out []string
	for _, x := range xs {
		if set[x] {
			out = append(out, x)
		}
	}
	return out
}

func minus(xs []string, set map[string]bool) []string {
	var out []string
	for _, x := range xs {
		if !set[x] {
			out = append(out, x)
		}
	}
	return out
}
