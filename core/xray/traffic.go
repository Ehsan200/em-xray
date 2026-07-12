package xray

import (
	"strconv"
	"strings"
	"time"
)

// InboundTag / OutboundTag are the xray routing tags for a user inbound/entry.
// Traffic counters are keyed by these tags, so the daemon reverses them back to
// display names via the same functions.
func InboundTag(name string) string  { return "in-" + sanitizeKey(name) }
func OutboundTag(name string) string { return "out-" + sanitizeKey(name) }

// TrafficBucket is one hour's byte delta for an inbound/outbound tag. The daemon
// upserts the current hour each poll, accumulating deltas since the last sample.
// Buckets are pruned after TrafficRetention; lifetime figures live in
// TrafficTotal.
type TrafficBucket struct {
	ID       uint   `gorm:"primaryKey"`
	Kind     string `gorm:"uniqueIndex:idx_traf_bucket,priority:1;not null"` // "inbound"|"outbound"
	Tag      string `gorm:"uniqueIndex:idx_traf_bucket,priority:2;not null"` // xray tag key
	HourUnix int64  `gorm:"uniqueIndex:idx_traf_bucket,priority:3;index;not null"`
	Up       int64
	Down     int64
}

// TrafficTotal is the lifetime byte total per tag. It survives bucket pruning
// and xray restarts, so "all time" figures are stable.
type TrafficTotal struct {
	ID        uint   `gorm:"primaryKey"`
	Kind      string `gorm:"uniqueIndex:idx_traf_total,priority:1;not null"`
	Tag       string `gorm:"uniqueIndex:idx_traf_total,priority:2;not null"`
	Name      string // last-known display name for the tag
	Up        int64
	Down      int64
	UpdatedAt time.Time
}

// TrafficRetention is how long per-hour buckets are kept (the chart window can't
// exceed this). Lifetime totals are never pruned.
const TrafficRetention = 8 * 24 * time.Hour

// TrafficKind constants.
const (
	KindInbound  = "inbound"
	KindOutbound = "outbound"
)

// StatCounter is one parsed xray traffic counter.
type StatCounter struct {
	Kind  string // "inbound"|"outbound"
	Tag   string // the middle segment (routing tag)
	Down  bool   // true=downlink, false=uplink
	Bytes int64
}

// ParseStatName parses an xray stat name of the form
// "inbound>>>TAG>>>traffic>>>uplink" (or outbound/downlink) into its parts. ok is
// false for any name that isn't a traffic counter (e.g. observatory/user stats).
func ParseStatName(name string, value int64) (StatCounter, bool) {
	parts := strings.Split(name, ">>>")
	if len(parts) != 4 || parts[2] != "traffic" {
		return StatCounter{}, false
	}
	kind := parts[0]
	if kind != KindInbound && kind != KindOutbound && kind != KindUser {
		return StatCounter{}, false
	}
	var down bool
	switch parts[3] {
	case "downlink":
		down = true
	case "uplink":
		down = false
	default:
		return StatCounter{}, false
	}
	return StatCounter{Kind: kind, Tag: parts[1], Down: down, Bytes: value}, true
}

// StatValueToInt coerces a stat value (xray emits it as string or number) to int64.
func StatValueToInt(v any) int64 {
	switch x := v.(type) {
	case float64:
		return int64(x)
	case string:
		n, _ := strconv.ParseInt(strings.TrimSpace(x), 10, 64)
		return n
	case int64:
		return x
	default:
		return 0
	}
}

// HourFloor truncates t to the start of its hour (UTC), as a unix timestamp.
func HourFloor(t time.Time) int64 {
	return t.UTC().Truncate(time.Hour).Unix()
}
