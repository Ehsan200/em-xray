package main

import "testing"

func TestParseSize(t *testing.T) {
	ok := map[string]int64{
		"":       0,
		"0":      0,
		"1024":   1024,
		"1KB":    1 << 10,
		"1K":     1 << 10,
		"10MB":   10 << 20,
		"1.5GB":  int64(1.5 * float64(1<<30)),
		"2TB":    2 << 40,
		"500 MB": 500 << 20,
	}
	for in, want := range ok {
		got, err := parseSize(in)
		if err != nil || got != want {
			t.Errorf("parseSize(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	for _, bad := range []string{"abc", "-5GB", "10XB"} {
		if _, err := parseSize(bad); err == nil {
			t.Errorf("parseSize(%q) should error", bad)
		}
	}
}
