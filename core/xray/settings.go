package xray

import (
	"encoding/json"
	"fmt"
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
// probe-traffic trade: the balancer skips a node that failed its whole last
// ping round (interval × DefaultProbeSampling), and after an xray restart no
// member is ranked until the first round lands.
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

// SettingAutoTuning holds the auto strategy's thresholds (JSON AutoTuning).
const SettingAutoTuning = "auto_strategy"

// AutoTuning are the thresholds the daemon's auto strategy uses to tell a
// pool whose nodes come and go in waves (switch it to agile) from a calm one
// (back to stable). Judged over the last WindowMin minutes of pool health: a
// pool is flapping when its whole pick died at once PickLoss times, or when
// FlappingPct percent of its nodes (and at least two) flipped alive↔dead
// Flips times or more. An agile pool goes back to stable after CalmMin
// minutes without flapping — slow on purpose, so a pool doesn't bounce.
type AutoTuning struct {
	WindowMin   int `json:"window_min"`
	Flips       int `json:"flips"`
	FlappingPct int `json:"flapping_pct"`
	PickLoss    int `json:"pick_loss"` // 0 = don't judge by pick losses
	CalmMin     int `json:"calm_min"`
}

// DefaultAutoTuning is the auto strategy's out-of-the-box thresholds.
var DefaultAutoTuning = AutoTuning{WindowMin: 10, Flips: 2, FlappingPct: 30, PickLoss: 2, CalmMin: 30}

// MaxAutoWindowMin is the longest judging window: the pool health history
// the daemon keeps.
const MaxAutoWindowMin = 30

// Validate reports a threshold out of range.
func (t AutoTuning) Validate() error {
	switch {
	case t.WindowMin < 2 || t.WindowMin > MaxAutoWindowMin:
		return fmt.Errorf("window must be 2-%d minutes", MaxAutoWindowMin)
	case t.Flips < 1 || t.Flips > 100:
		return fmt.Errorf("flips must be 1-100")
	case t.FlappingPct < 1 || t.FlappingPct > 100:
		return fmt.Errorf("flapping share must be 1-100%%")
	case t.PickLoss < 0 || t.PickLoss > 100:
		return fmt.Errorf("pick losses must be 0-100")
	case t.CalmMin < 1 || t.CalmMin > 24*60:
		return fmt.Errorf("calm time must be 1-1440 minutes")
	}
	return nil
}

// AutoTuning returns the stored thresholds, or the defaults when unset/bad.
func (s *Store) AutoTuning() AutoTuning {
	if v, ok := s.GetSetting(SettingAutoTuning); ok {
		var t AutoTuning
		if json.Unmarshal([]byte(v), &t) == nil && t.Validate() == nil {
			return t
		}
	}
	return DefaultAutoTuning
}

// SetAutoTuning validates and stores the thresholds.
func (s *Store) SetAutoTuning(t AutoTuning) error {
	if err := t.Validate(); err != nil {
		return err
	}
	b, _ := json.Marshal(t)
	return s.SetSetting(SettingAutoTuning, string(b))
}
