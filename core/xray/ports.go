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

	DefaultApiPort = 11932 // gRPC api (dokodemo-door) default
	ApiTag         = "api"
)

// ApiPort is the loopback port for xray's gRPC api inbound. It's a var (not a
// const) so the daemon can move it off a busy default — e.g. when another xray
// (the user's em-wall) already holds 11932. Config generation and every
// `xray api` call read this same value, so they stay consistent per process.
var ApiPort = DefaultApiPort

// AssignInboundPorts allocates a stable listen port to every enabled TCP inbound
// that lacks one (Port==0), choosing the lowest free port in [PortStart,PortEnd]
// not already taken by another inbound. Unix-socket inbounds keep Port at zero.
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
		if !inbounds[i].Enabled || inbounds[i].Port != 0 || IsUnixInbound(inbounds[i]) {
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
