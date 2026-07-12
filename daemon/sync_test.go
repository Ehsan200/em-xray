package daemon

import (
	"sort"
	"testing"

	"github.com/gravisun/em-xray/core/xray"
)

func TestComputeDelta(t *testing.T) {
	loaded := map[string]bool{"a": true, "b": true}
	desired := []xray.SlotMember{{Key: "b"}, {Key: "c"}}
	adds, removes := computeDelta(loaded, desired)

	if len(adds) != 1 || adds[0].Key != "c" {
		t.Errorf("adds = %v, want [c]", adds)
	}
	sort.Strings(removes)
	if len(removes) != 1 || removes[0] != "a" {
		t.Errorf("removes = %v, want [a]", removes)
	}

	// No change.
	adds, removes = computeDelta(map[string]bool{"x": true}, []xray.SlotMember{{Key: "x"}})
	if len(adds) != 0 || len(removes) != 0 {
		t.Errorf("expected no delta, got +%v -%v", adds, removes)
	}
}

func TestSameSlotMasters(t *testing.T) {
	loaded := map[string]loadedSlot{"M": {}, "N": {}}
	if !sameSlotMasters([]xray.Slot{{Master: "M"}, {Master: "N"}}, loaded) {
		t.Error("same master set should match")
	}
	if sameSlotMasters([]xray.Slot{{Master: "M"}}, loaded) {
		t.Error("fewer masters should not match")
	}
	if sameSlotMasters([]xray.Slot{{Master: "M"}, {Master: "Z"}}, loaded) {
		t.Error("different master should not match")
	}
}
