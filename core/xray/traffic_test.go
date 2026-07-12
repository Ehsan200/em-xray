package xray

import (
	"testing"
	"time"
)

func TestParseStatName(t *testing.T) {
	cases := []struct {
		name   string
		wantOK bool
		kind   string
		tag    string
		down   bool
	}{
		{"inbound>>>in-srv>>>traffic>>>downlink", true, KindInbound, "in-srv", true},
		{"outbound>>>out-alpha>>>traffic>>>uplink", true, KindOutbound, "out-alpha", false},
		{"outbound>>>direct>>>traffic>>>downlink", true, KindOutbound, "direct", true},
		{"inbound>>>api>>>traffic>>>uplink", true, KindInbound, "api", false},
		{"user>>>srv.alice>>>traffic>>>uplink", true, KindUser, "srv.alice", false},
		{"observatory>>>foo", false, "", "", false},
		{"inbound>>>x>>>traffic>>>sideways", false, "", "", false},
	}
	for _, c := range cases {
		got, ok := ParseStatName(c.name, 42)
		if ok != c.wantOK {
			t.Errorf("%q ok=%v want %v", c.name, ok, c.wantOK)
			continue
		}
		if !ok {
			continue
		}
		if got.Kind != c.kind || got.Tag != c.tag || got.Down != c.down || got.Bytes != 42 {
			t.Errorf("%q => %+v", c.name, got)
		}
	}
}

func TestStatValueToInt(t *testing.T) {
	if StatValueToInt("1234") != 1234 || StatValueToInt(float64(56)) != 56 || StatValueToInt(int64(7)) != 7 {
		t.Error("StatValueToInt coercion wrong")
	}
	if StatValueToInt(nil) != 0 || StatValueToInt("x") != 0 {
		t.Error("bad values should coerce to 0")
	}
}

func TestHourFloor(t *testing.T) {
	tm := time.Date(2026, 7, 12, 14, 37, 5, 0, time.UTC)
	if got := HourFloor(tm); got != time.Date(2026, 7, 12, 14, 0, 0, 0, time.UTC).Unix() {
		t.Errorf("HourFloor = %d", got)
	}
}

func TestAddTrafficUpsertAndTotals(t *testing.T) {
	s := memStore(t)
	h := HourFloor(time.Now())

	// Two deltas in the same hour accumulate into one bucket + the total.
	if err := s.AddTraffic(KindInbound, "in-srv", "srv", h, 100, 900); err != nil {
		t.Fatal(err)
	}
	if err := s.AddTraffic(KindInbound, "in-srv", "srv", h, 50, 100); err != nil {
		t.Fatal(err)
	}
	bs, err := s.TrafficBucketsSince(KindInbound, "in-srv", h)
	if err != nil {
		t.Fatal(err)
	}
	if len(bs) != 1 || bs[0].Up != 150 || bs[0].Down != 1000 {
		t.Fatalf("bucket = %+v", bs)
	}
	totals, err := s.TrafficTotals()
	if err != nil {
		t.Fatal(err)
	}
	if len(totals) != 1 || totals[0].Up != 150 || totals[0].Down != 1000 || totals[0].Name != "srv" {
		t.Fatalf("total = %+v", totals)
	}

	// A later hour makes a second bucket; total keeps growing.
	if err := s.AddTraffic(KindInbound, "in-srv", "srv", h+3600, 10, 20); err != nil {
		t.Fatal(err)
	}
	bs, _ = s.TrafficBucketsSince(KindInbound, "in-srv", h)
	if len(bs) != 2 {
		t.Fatalf("want 2 buckets, got %d", len(bs))
	}
	totals, _ = s.TrafficTotals()
	if totals[0].Down != 1020 {
		t.Errorf("lifetime total should accumulate across hours: %+v", totals[0])
	}
}

func TestPruneTrafficBefore(t *testing.T) {
	s := memStore(t)
	h := HourFloor(time.Now())
	old := h - 100*3600
	s.AddTraffic(KindOutbound, "direct", "direct", old, 1, 1)
	s.AddTraffic(KindOutbound, "direct", "direct", h, 2, 2)
	if err := s.PruneTrafficBefore(h); err != nil {
		t.Fatal(err)
	}
	bs, _ := s.TrafficBucketsSince(KindOutbound, "direct", old)
	if len(bs) != 1 || bs[0].HourUnix != h {
		t.Errorf("prune should drop the old bucket, kept: %+v", bs)
	}
	// Lifetime total survives pruning.
	totals, _ := s.TrafficTotals()
	if len(totals) != 1 || totals[0].Down != 3 {
		t.Errorf("total must survive prune: %+v", totals)
	}
}
