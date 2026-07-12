package xray

import (
	"testing"
	"time"
)

func TestInboundUserCRUDAndEmail(t *testing.T) {
	s := memStore(t)
	in, err := NewInboundFromTemplate("srv", "vless-reality", "direct")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.CreateInbound(in); err != nil {
		t.Fatal(err)
	}

	u, err := NewInboundUser(in, "alice", 10<<20)
	if err != nil {
		t.Fatal(err)
	}
	if u.UUID == "" {
		t.Error("vless user must get a uuid")
	}
	if u.Email != "srv.alice" {
		t.Errorf("email = %q, want srv.alice", u.Email)
	}
	if err := s.CreateInboundUser(u); err != nil {
		t.Fatal(err)
	}

	users, err := s.InboundUsers(in.ID)
	if err != nil || len(users) != 1 || users[0].Name != "alice" {
		t.Fatalf("InboundUsers = %v, %v", users, err)
	}

	// Duplicate name on the same inbound is rejected by the unique index.
	dup, _ := NewInboundUser(in, "alice", 0)
	if err := s.CreateInboundUser(dup); err == nil {
		t.Error("duplicate user name on same inbound should fail")
	}

	if err := s.DeleteInboundUser(u.ID); err != nil {
		t.Fatal(err)
	}
	users, _ = s.InboundUsers(in.ID)
	if len(users) != 0 {
		t.Error("user should be deleted")
	}
}

func TestTrafficTotalFor(t *testing.T) {
	s := memStore(t)
	h := HourFloor(time.Now())
	s.AddTraffic(KindUser, "srv.bob", "srv/bob", h, 100, 400)
	up, down := s.TrafficTotalFor(KindUser, "srv.bob")
	if up != 100 || down != 400 {
		t.Errorf("TrafficTotalFor = %d,%d", up, down)
	}
	if u, d := s.TrafficTotalFor(KindUser, "missing"); u != 0 || d != 0 {
		t.Errorf("missing tag should be 0,0, got %d,%d", u, d)
	}
}
