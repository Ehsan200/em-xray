package xray

import (
	"regexp"
	"strings"
)

// Loopback port map. Everything binds 127.0.0.1.
const (
	PortStart = 11800 // per-entry SOCKS inbounds
	PortEnd   = 11899

	SlotPortStart = 11900 // slot inbounds; SlotCount masters max
	SlotCount     = 32
	SlotPortEnd   = SlotPortStart + SlotCount - 1 // 11931

	ApiPort = 11932 // gRPC api (dokodemo-door), only when a slot exists
	ApiTag  = "api"
)

// AssignInboundPorts allocates a stable listen port to every enabled inbound
// that lacks one (Port==0), choosing the lowest free port in [PortStart,PortEnd]
// not already taken by another inbound. Inbounds with an explicit Port keep it.
// It mutates inbounds in place and returns the indices whose Port changed.
func AssignInboundPorts(inbounds []Inbound) []int {
	used := make(map[int]bool)
	for i := range inbounds {
		if inbounds[i].Port != 0 {
			used[inbounds[i].Port] = true
		}
	}
	var changed []int
	for i := range inbounds {
		if !inbounds[i].Enabled || inbounds[i].Port != 0 {
			continue
		}
		p := nextFreePort(used)
		if p == 0 {
			break // range exhausted; leave unassigned (Generate skips it)
		}
		inbounds[i].Port = p
		used[p] = true
		changed = append(changed, i)
	}
	return changed
}

func nextFreePort(used map[int]bool) int {
	for p := PortStart; p <= PortEnd; p++ {
		if !used[p] {
			return p
		}
	}
	return 0
}

var nonTagRe = regexp.MustCompile(`[^A-Za-z0-9_-]+`)

// sanitizeKey turns an arbitrary name into a tag-safe token (routing tags must
// match exactly, so keep them to [A-Za-z0-9_-]). Collisions are prevented
// upstream by unique entry/subscription names.
func sanitizeKey(s string) string {
	s = nonTagRe.ReplaceAllString(strings.TrimSpace(s), "_")
	return strings.Trim(s, "_")
}
