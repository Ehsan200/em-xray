package xray

import "regexp"

// slotOutRe matches a slot member outbound tag: slot<idx>-out-<key>.
var slotOutRe = regexp.MustCompile(`slot(\d+)-out-[A-Za-z0-9_-]+`)

// ParseBalancerSelected extracts the members one balancer selects, in the
// order xray ranks them, from the human text of `xray api bi` for that one
// balancer. That output is TEXT, not JSON, and its labels ("Selects:",
// "Principle target:") vary across xray versions, so it keys off the member
// tags themselves. Every master on a slot has its own balancer over the same
// members, so the text must be one balancer's: the tags can't tell them
// apart. Duplicates are dropped.
func ParseBalancerSelected(text string) []string {
	var sel []string
	seen := map[string]bool{}
	for _, m := range slotOutRe.FindAllString(text, -1) {
		if !seen[m] {
			seen[m] = true
			sel = append(sel, m)
		}
	}
	return sel
}
