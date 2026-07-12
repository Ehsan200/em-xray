package xray

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestParseUserinfo(t *testing.T) {
	q := ParseUserinfo("upload=100; download=200; total=1000; expire=1699999999")
	if !q.HasData || q.Upload != 100 || q.Download != 200 || q.Total != 1000 || q.Expire != 1699999999 {
		t.Errorf("bad quota: %+v", q)
	}
	if ParseUserinfo("").HasData {
		t.Error("empty header should have no data")
	}
}

func TestDecodeSubscription(t *testing.T) {
	links := "vless://u@a.com:443#A\ntrojan://p@b.com:443#B\n\n# junk line without scheme\n"
	// plaintext
	got := DecodeSubscription([]byte(links))
	if len(got) != 2 {
		t.Fatalf("plaintext: got %d links, want 2: %v", len(got), got)
	}
	// base64-wrapped
	b64 := base64.StdEncoding.EncodeToString([]byte(links))
	got2 := DecodeSubscription([]byte(b64))
	if len(got2) != 2 || got2[0] != "vless://u@a.com:443#A" {
		t.Fatalf("base64: got %v", got2)
	}
}

func TestParseSubscriptionBodyDedupePrefix(t *testing.T) {
	// Two identical outbounds differing only by #name must dedupe to one node.
	body := "vless://u@a.com:443?type=ws&path=%2Fp#One\n" +
		"vless://u@a.com:443?type=ws&path=%2Fp#Two\n" +
		"trojan://pw@b.com:443#Other\n"
	nodes, err := ParseSubscriptionBody("MySub", []byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if len(nodes) != 2 {
		t.Fatalf("got %d nodes, want 2 (dedupe by fingerprint)", len(nodes))
	}
	if !strings.HasPrefix(nodes[0].Name, "MySub / ") {
		t.Errorf("name not prefixed: %q", nodes[0].Name)
	}
}

func TestFetchSubscriptionBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("User-Agent") != "v2rayN/6.23" {
			t.Errorf("UA = %q", r.Header.Get("User-Agent"))
		}
		w.Header().Set("Subscription-Userinfo", "upload=1; download=2; total=3; expire=4")
		w.Write([]byte("vless://u@a.com:443#A"))
	}))
	defer srv.Close()

	body, q, err := FetchSubscriptionBody(context.Background(), srv.Client(), srv.URL, "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "vless://") {
		t.Errorf("body = %q", body)
	}
	if !q.HasData || q.Total != 3 {
		t.Errorf("quota = %+v", q)
	}
}

func TestFetchSizeCap(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(make([]byte, MaxSubBytes+10))
	}))
	defer srv.Close()
	if _, _, err := FetchSubscriptionBody(context.Background(), srv.Client(), srv.URL, ""); err == nil {
		t.Error("oversize body should error")
	}
}

func TestIsDue(t *testing.T) {
	now := time.Now()
	// never fetched
	if !IsDue(Subscription{Enabled: true}, now) {
		t.Error("never-fetched enabled sub should be due")
	}
	// disabled
	if IsDue(Subscription{Enabled: false, LastFetched: now.Add(-100 * time.Hour)}, now) {
		t.Error("disabled sub never due")
	}
	// within interval
	if IsDue(Subscription{Enabled: true, IntervalSec: 3600, LastFetched: now.Add(-10 * time.Minute)}, now) {
		t.Error("within interval should not be due")
	}
	// past interval
	if !IsDue(Subscription{Enabled: true, IntervalSec: 3600, LastFetched: now.Add(-2 * time.Hour)}, now) {
		t.Error("past interval should be due")
	}
	// error retry: recent error not due, old error due
	if IsDue(Subscription{Enabled: true, LastError: "x", LastFetched: now.Add(-1 * time.Minute)}, now) {
		t.Error("recent error within retry window should not be due")
	}
	if !IsDue(Subscription{Enabled: true, LastError: "x", LastFetched: now.Add(-10 * time.Minute)}, now) {
		t.Error("old error past retry window should be due")
	}
}
