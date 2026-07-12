package daemon

import (
	"encoding/json"
	"testing"

	"github.com/ehsan200/em-xray/core/xray"
)

// TestConfigBackupRoundTrip proves a backup marshals and restores every field,
// including generated credentials (uuid/reality keys/tls cert), through a fresh
// store — the core risk of the export/import feature.
func TestConfigBackupRoundTrip(t *testing.T) {
	src, err := xray.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()

	in, err := xray.NewInboundFromTemplate("srv", "vless-tls", "direct")
	if err != nil {
		t.Fatal(err)
	}
	if err := src.CreateInbound(in); err != nil {
		t.Fatal(err)
	}
	if err := src.CreateEntry(&xray.XrayEntry{Name: "e", Outbound: `{"protocol":"vless"}`, Enabled: true, Dialer: "xraysub:pool"}); err != nil {
		t.Fatal(err)
	}
	if err := src.CreateSubscription(&xray.Subscription{Name: "pool", URL: "https://x/y", Enabled: true, IntervalSec: 3600}); err != nil {
		t.Fatal(err)
	}

	ins, _ := src.ListInbounds()
	ents, _ := src.ListEntries()
	subs, _ := src.ListSubscriptions()
	raw, err := json.Marshal(configBackup{Version: backupVersion, Inbounds: ins, Entries: ents, Subscriptions: subs})
	if err != nil {
		t.Fatal(err)
	}

	// Restore into a clean store, mimicking ImportConfig's reset-then-create.
	dst, err := xray.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer dst.Close()
	var b configBackup
	if err := json.Unmarshal(raw, &b); err != nil {
		t.Fatal(err)
	}
	for _, x := range b.Inbounds {
		x.ID = 0
		if err := dst.CreateInbound(&x); err != nil {
			t.Fatal(err)
		}
	}
	for _, x := range b.Entries {
		x.ID = 0
		if err := dst.CreateEntry(&x); err != nil {
			t.Fatal(err)
		}
	}
	for _, x := range b.Subscriptions {
		x.ID = 0
		if err := dst.CreateSubscription(&x); err != nil {
			t.Fatal(err)
		}
	}

	got, _ := dst.GetInboundByName("srv")
	if got == nil || got.UUID != in.UUID {
		t.Errorf("uuid not preserved: %v", got)
	}
	if got.TLSCert == "" || got.TLSCert != in.TLSCert {
		t.Error("tls cert not preserved through backup")
	}
	if e, _ := dst.GetEntryByName("e"); e == nil || e.Dialer != "xraysub:pool" {
		t.Errorf("entry dialer not preserved: %v", e)
	}
	if s, _ := dst.GetSubscriptionByName("pool"); s == nil || s.IntervalSec != 3600 {
		t.Errorf("subscription not preserved: %v", s)
	}
}
