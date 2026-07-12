package xray

import (
	"fmt"
	"regexp"
)

// slotOutRe matches a slot member outbound tag: slot<idx>-out-<key>.
var slotOutRe = regexp.MustCompile(`slot(\d+)-out-[A-Za-z0-9_-]+`)

// ParseBalancerWinners extracts the current winner per balancer from the human
// text of `xray api bi`. That output is TEXT, not JSON, and exposes only each
// balancer's selected member — no per-node RTT over the CLI.
//
// Rather than depend on the exact labels ("Selects:", "Principle target:",
// which vary across xray versions), it keys off the winner tag itself: a
// slot<idx>-out-<key> token implies balancer slot<idx>-bal. The FIRST such token
// per slot index is taken as the winner. Returns balancerTag → winnerTag.
func ParseBalancerWinners(text string) map[string]string {
	winners := map[string]string{}
	for _, m := range slotOutRe.FindAllStringSubmatch(text, -1) {
		full, idx := m[0], m[1]
		bal := fmt.Sprintf("slot%s-bal", idx)
		if _, seen := winners[bal]; !seen {
			winners[bal] = full
		}
	}
	return winners
}
