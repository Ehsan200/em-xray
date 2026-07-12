package xray

import (
	"encoding/json"
	"testing"
)

func TestSettingsDefaultsAndSet(t *testing.T) {
	s := memStore(t)
	if s.LogLevel() != DefaultLogLevel {
		t.Errorf("default loglevel = %q", s.LogLevel())
	}
	if s.LogMaxMB() != DefaultLogMaxMB {
		t.Errorf("default logmax = %d", s.LogMaxMB())
	}
	if s.TrafficDays() != DefaultTrafficDays {
		t.Errorf("default traffic days = %d", s.TrafficDays())
	}

	if err := s.SetSetting(SettingLogLevel, "debug"); err != nil {
		t.Fatal(err)
	}
	if s.LogLevel() != "debug" {
		t.Errorf("loglevel after set = %q", s.LogLevel())
	}
	// Upsert (change existing).
	if err := s.SetSetting(SettingLogLevel, "error"); err != nil {
		t.Fatal(err)
	}
	if s.LogLevel() != "error" {
		t.Errorf("loglevel after re-set = %q", s.LogLevel())
	}
	// Invalid stored value falls back to default.
	s.SetSetting(SettingLogLevel, "bogus")
	if s.LogLevel() != DefaultLogLevel {
		t.Errorf("invalid loglevel should fall back, got %q", s.LogLevel())
	}

	s.SetSetting(SettingLogMaxMB, "10")
	if s.LogMaxMB() != 10 {
		t.Errorf("logmax = %d", s.LogMaxMB())
	}
	s.SetSetting(SettingTrafficDays, "3")
	if s.TrafficDays() != 3 {
		t.Errorf("traffic days = %d", s.TrafficDays())
	}
}

func TestValidLogLevel(t *testing.T) {
	for _, ok := range LogLevels() {
		if !ValidLogLevel(ok) {
			t.Errorf("%q should be valid", ok)
		}
	}
	if ValidLogLevel("verbose") {
		t.Error("verbose is not a valid xray level")
	}
}

func TestGenerateLogLevel(t *testing.T) {
	b, err := Generate(nil, nil, nil, GenOptions{LogLevel: "debug"})
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	json.Unmarshal(b, &m)
	if lvl := dig(t, m, "log", "loglevel"); lvl != "debug" {
		t.Errorf("config loglevel = %v, want debug", lvl)
	}
	// Empty defaults to warning.
	b2, _ := Generate(nil, nil, nil, GenOptions{})
	var m2 map[string]any
	json.Unmarshal(b2, &m2)
	if lvl := dig(t, m2, "log", "loglevel"); lvl != "warning" {
		t.Errorf("default loglevel = %v, want warning", lvl)
	}
}
