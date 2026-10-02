package xray

import (
	"reflect"
	"testing"
)

// Real `xray api bi` output for a leastLoad balancer with expected=2.
func TestParseBalancerSelected(t *testing.T) {
	text := "  - Selecting Override:\n    1                 \n  - Selects:\n    1   slot0-out-xray-good2\n    2   slot0-out-xray-good1\n"
	if got := ParseBalancerSelected(text); !reflect.DeepEqual(got, []string{"slot0-out-xray-good2", "slot0-out-xray-good1"}) {
		t.Fatalf("selects = %v", got)
	}
}

func TestParseBalancerSelectedDedupes(t *testing.T) {
	// Tolerant of exact bi text format — keys off the slotN-out- token.
	text := "Balancer: slot0-bal-M\n  Selects:\n    - slot0-out-aaa\n    - slot0-out-bbb\n  Fallback: slot0-out-aaa\n"
	if got := ParseBalancerSelected(text); !reflect.DeepEqual(got, []string{"slot0-out-aaa", "slot0-out-bbb"}) {
		t.Errorf("selects = %v", got)
	}
}

func TestParseBalancerSelectedEmpty(t *testing.T) {
	if got := ParseBalancerSelected("no tags here"); len(got) != 0 {
		t.Errorf("expected none, got %v", got)
	}
}
