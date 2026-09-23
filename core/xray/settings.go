package xray

import (
	"strconv"

	"gorm.io/gorm/clause"
)

// Setting is a persisted key/value daemon setting (e.g. the xray log level).
type Setting struct {
	Key   string `gorm:"primaryKey"`
	Value string
}

// Setting keys.
const (
	SettingLogLevel      = "loglevel"
	SettingLogMaxMB      = "log_max_mb"
	SettingTrafficDays   = "traffic_days"
	SettingProbeInterval = "probe_interval_sec"
)

// Burst-observatory ping cadence bounds. The interval is a reaction-time /
// probe-traffic trade: leastLoad drops a failing node on its next ping, and
// after an xray restart no member is ranked until the first ping lands.
const (
	DefaultProbeIntervalSec = 10
	MinProbeIntervalSec     = 5
	MaxProbeIntervalSec     = 3600
)

// DefaultLogLevel is xray's log level when none is set.
const DefaultLogLevel = "warning"

// DefaultLogMaxMB caps each xray log file; the rotator truncates past it.
const DefaultLogMaxMB = 50

// DefaultTrafficDays is how many days of hourly traffic buckets are kept.
const DefaultTrafficDays = 8

// validLogLevels are the levels xray accepts.
var validLogLevels = map[string]bool{
	"debug": true, "info": true, "warning": true, "error": true, "none": true,
}

// ValidLogLevel reports whether s is a log level xray understands.
func ValidLogLevel(s string) bool { return validLogLevels[s] }

// LogLevels returns the accepted log levels (for help text), fixed order.
func LogLevels() []string { return []string{"debug", "info", "warning", "error", "none"} }

// GetSetting returns a setting's value and whether it was present.
func (s *Store) GetSetting(key string) (string, bool) {
	var st Setting
	if err := s.db.First(&st, "key = ?", key).Error; err != nil {
		return "", false
	}
	return st.Value, true
}

// SetSetting upserts a setting.
func (s *Store) SetSetting(key, value string) error {
	return s.db.Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "key"}},
		DoUpdates: clause.AssignmentColumns([]string{"value"}),
	}).Create(&Setting{Key: key, Value: value}).Error
}

// LogLevel returns the configured xray log level, or the default.
func (s *Store) LogLevel() string {
	if v, ok := s.GetSetting(SettingLogLevel); ok && ValidLogLevel(v) {
		return v
	}
	return DefaultLogLevel
}

// LogMaxMB returns the per-file log size cap in MB (0 => disabled), or the
// default when unset.
func (s *Store) LogMaxMB() int {
	if v, ok := s.GetSetting(SettingLogMaxMB); ok {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			return n
		}
	}
	return DefaultLogMaxMB
}

// ProbeIntervalSec returns the observatory probe cadence in seconds, clamped to
// the supported range, or the default when unset.
func (s *Store) ProbeIntervalSec() int {
	if v, ok := s.GetSetting(SettingProbeInterval); ok {
		if n, err := strconv.Atoi(v); err == nil && n >= MinProbeIntervalSec && n <= MaxProbeIntervalSec {
			return n
		}
	}
	return DefaultProbeIntervalSec
}

// TrafficDays returns how many days of hourly traffic buckets to retain.
func (s *Store) TrafficDays() int {
	if v, ok := s.GetSetting(SettingTrafficDays); ok {
		if n, err := strconv.Atoi(v); err == nil && n >= 1 {
			return n
		}
	}
	return DefaultTrafficDays
}
