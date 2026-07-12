package xray

import "testing"

func TestDuplicateInboundFreshCreds(t *testing.T) {
	s := memStore(t)
	src, err := NewInboundFromTemplate("srv", "vless-reality", "direct")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.CreateInbound(src); err != nil {
		t.Fatal(err)
	}

	dup, err := s.DuplicateInbound(src.ID, "srv copy")
	if err != nil {
		t.Fatal(err)
	}
	if dup.ID == src.ID {
		t.Error("duplicate must be a new row")
	}
	if dup.Name != "srv copy" {
		t.Errorf("name = %q", dup.Name)
	}
	if dup.UUID == src.UUID {
		t.Error("uuid must be regenerated")
	}
	if dup.RealityPrivateKey == src.RealityPrivateKey || dup.RealityShortID == src.RealityShortID {
		t.Error("reality keys/shortId must be regenerated")
	}
	if dup.Port == 0 || dup.Port == src.Port {
		t.Errorf("duplicate must get its own port: src=%d dup=%d", src.Port, dup.Port)
	}
	// Shape is preserved.
	if dup.Protocol != src.Protocol || dup.Security != src.Security || dup.Target != src.Target {
		t.Error("protocol/security/target must be preserved")
	}
}

func TestDuplicateInboundTLSFreshCert(t *testing.T) {
	s := memStore(t)
	src, err := NewInboundFromTemplate("tls", "vless-tls", "direct")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.CreateInbound(src); err != nil {
		t.Fatal(err)
	}
	dup, err := s.DuplicateInbound(src.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	if dup.TLSCert == "" || dup.TLSCert == src.TLSCert {
		t.Error("tls cert must be regenerated for the copy")
	}
}

func TestDuplicateEntryVerbatim(t *testing.T) {
	s := memStore(t)
	src := &XrayEntry{Name: "e", Outbound: `{"protocol":"vless"}`, Enabled: true, Dialer: "xraysub:pool"}
	if err := s.CreateEntry(src); err != nil {
		t.Fatal(err)
	}
	dup, err := s.DuplicateEntry(src.ID, "e2")
	if err != nil {
		t.Fatal(err)
	}
	if dup.Outbound != src.Outbound || dup.Dialer != src.Dialer {
		t.Error("entry outbound + dialer must be copied verbatim")
	}
	if dup.ID == src.ID {
		t.Error("duplicate must be a new row")
	}
}
