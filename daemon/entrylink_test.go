package daemon

import (
	"context"
	"strings"
	"testing"

	emxv1 "github.com/ehsan200/em-xray/api/emxv1"
	"github.com/ehsan200/em-xray/core/xray"
)

// An entry's share link is rebuilt from its current outbound, so an edit after
// adding shows up in the exported link; outbounds with no link form say why.
func TestEntryShareLinkFollowsEdits(t *testing.T) {
	store, sup := newTestSupervisor(t)
	srv := &Server{store: store, sup: sup}
	ctx := context.Background()

	link := "vless://11111111-2222-3333-4444-555555555555@198.51.100.1:443?type=ws&security=tls&sni=a.example.com&path=%2Fws#vps"
	added, err := srv.EntryAdd(ctx, &emxv1.EntryAddRequest{Link: link})
	if err != nil {
		t.Fatal(err)
	}
	if got := added.Entry.ShareLink; !strings.HasPrefix(got, "vless://11111111-2222-3333-4444-555555555555@198.51.100.1:443?") || !strings.HasSuffix(got, "#vps") {
		t.Fatalf("share link after add: %q", got)
	}

	cfg, err := srv.EntryGetConfig(ctx, &emxv1.IdRequest{Id: added.Entry.Id})
	if err != nil {
		t.Fatal(err)
	}
	edited := strings.Replace(cfg.Json, "198.51.100.1", "198.51.100.2", 1)
	if _, err := srv.EntrySetConfig(ctx, &emxv1.SetConfigRequest{Id: added.Entry.Id, Json: edited}); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateEntry(&xray.XrayEntry{Name: "direct", Enabled: true, Outbound: `{"protocol":"freedom"}`}); err != nil {
		t.Fatal(err)
	}

	list, err := srv.EntryList(ctx, &emxv1.Empty{})
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range list.Entries {
		switch e.Name {
		case "vps":
			if !strings.Contains(e.ShareLink, "@198.51.100.2:443?") {
				t.Errorf("edit not reflected: %q", e.ShareLink)
			}
		case "direct":
			if e.ShareLink != "" || !strings.Contains(e.ShareLinkNote, "freedom") {
				t.Errorf("freedom: link %q note %q", e.ShareLink, e.ShareLinkNote)
			}
		}
	}
}
