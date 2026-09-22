package main

import (
	"strings"
	"testing"

	emxv1 "github.com/ehsan200/em-xray/api/emxv1"
)

func TestRenderCaddyfileGroupsDomains(t *testing.T) {
	inbounds := []*emxv1.InboundInfo{
		{Name: "b", Enabled: true, Network: "xhttp", Security: "none", Listen: "/dev/shm/b.sock,0666", PublicHost: "x.example.com", Path: "/b/"},
		{Name: "a", Enabled: true, Network: "xhttp", Security: "none", Listen: "/dev/shm/a.sock,0666", PublicHost: "x.example.com", Path: "/a/"},
		{Name: "off", Enabled: false, Network: "xhttp", Security: "none", Listen: "/dev/shm/off.sock,0666", PublicHost: "off.example.com", Path: "/off/"},
		{Name: "tcp", Enabled: true, Network: "xhttp", Security: "tls", Listen: "0.0.0.0", Port: 11800, PublicHost: "tcp.example.com", Path: "/"},
	}
	cfg, managed, err := renderCaddyfile(inbounds)
	if err != nil {
		t.Fatal(err)
	}
	if len(managed) != 2 {
		t.Fatalf("managed = %d, want 2", len(managed))
	}
	if strings.Count(cfg, "{\n") != 1 {
		t.Fatalf("same-domain routes must share one site block:\n%s", cfg)
	}
	for _, want := range []string{"http://x.example.com:80", "http://x.example.com:2095", "https://x.example.com:443", "https://x.example.com:8443"} {
		if !strings.Contains(cfg, want) {
			t.Errorf("Cloudflare address missing %q:\n%s", want, cfg)
		}
	}
	for _, want := range []string{"path /a/*", "unix//dev/shm/a.sock", "path /b/*", "unix//dev/shm/b.sock"} {
		if !strings.Contains(cfg, want) {
			t.Errorf("config missing %q:\n%s", want, cfg)
		}
	}
	if strings.Contains(cfg, "off.example.com") || strings.Contains(cfg, "tcp.example.com") {
		t.Fatalf("non-managed inbound leaked into config:\n%s", cfg)
	}
}

func TestRenderCaddyfileRejectsUnsafeValues(t *testing.T) {
	makeInbound := func(domain, path string) *emxv1.InboundInfo {
		return &emxv1.InboundInfo{Name: "x", Enabled: true, Network: "xhttp", Security: "none", Listen: "/tmp/x.sock,0666", PublicHost: domain, Path: path}
	}
	if _, _, err := renderCaddyfile([]*emxv1.InboundInfo{makeInbound("x.example.com {", "/x/")}); err == nil {
		t.Fatal("unsafe domain accepted")
	}
	if _, _, err := renderCaddyfile([]*emxv1.InboundInfo{makeInbound("x.example.com", "/x/\nrespond 200")}); err == nil {
		t.Fatal("unsafe path accepted")
	}
	badSocket := makeInbound("x.example.com", "/x/")
	badSocket.Listen = "/tmp/x.sock\nrespond 200"
	if _, _, err := renderCaddyfile([]*emxv1.InboundInfo{badSocket}); err == nil {
		t.Fatal("unsafe socket accepted")
	}
	if _, _, err := renderCaddyfile([]*emxv1.InboundInfo{makeInbound("x.example.com", "/x/"), makeInbound("x.example.com", "/x/")}); err == nil {
		t.Fatal("duplicate route accepted")
	}
}
