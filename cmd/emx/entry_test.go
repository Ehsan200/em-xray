package main

import (
	"testing"

	emxv1 "github.com/ehsan200/em-xray/api/emxv1"
)

func TestEditDialer(t *testing.T) {
	cases := []struct {
		name         string
		cur          string
		set, add, rm []string
		clear        bool
		want         string
		wantErr      bool
	}{
		{name: "replace", cur: "xraysub:a", set: []string{"xraysub:b", "xray:c"}, want: "xraysub:b,xray:c"},
		{name: "replace comma list", cur: "", set: []string{"xraysub:a,xraysub:b"}, want: "xraysub:a,xraysub:b"},
		{name: "add keeps order, dedupes", cur: "xraysub:a", add: []string{"xraysub:b", "xraysub:a"}, want: "xraysub:a,xraysub:b"},
		{name: "rm", cur: "xraysub:a, xraysub:b", rm: []string{"xraysub:a"}, want: "xraysub:b"},
		{name: "rm missing", cur: "xraysub:a", rm: []string{"xraysub:z"}, wantErr: true},
		{name: "clear", cur: "xraysub:a,xray:c", clear: true, want: ""},
		{name: "clear then add", cur: "xraysub:a", clear: true, add: []string{"xray:c"}, want: "xray:c"},
		{name: "clear with refs", cur: "xraysub:a", clear: true, set: []string{"xray:c"}, wantErr: true},
	}
	for _, c := range cases {
		got, err := editDialer(c.cur, c.set, c.add, c.rm, c.clear)
		if (err != nil) != c.wantErr || got != c.want {
			t.Errorf("%s: got %q, %v; want %q, err=%v", c.name, got, err, c.want, c.wantErr)
		}
	}
}

func TestBulkDialerEdit(t *testing.T) {
	entries := []*emxv1.EntryInfo{
		{Id: 1, Name: "a", IsMaster: true, Dialer: "xraysub:x,xraysub:y"},
		{Id: 2, Name: "b", IsMaster: true, Dialer: "xraysub:x"},
		{Id: 3, Name: "c"},
	}
	if sel, err := selectEntries(entries, "masters"); err != nil || len(sel) != 2 {
		t.Fatalf("masters: %v %v", sel, err)
	}
	if sel, err := selectEntries(entries, "3,1,3"); err != nil || len(sel) != 2 || sel[0].Id != 3 {
		t.Fatalf("ids: %v %v", sel, err)
	}
	if _, err := selectEntries(entries, "9"); err == nil {
		t.Fatal("unknown id accepted")
	}

	// --rm skips entries without the ref; --add appends everywhere.
	items, err := editDialers(entries, nil, []string{"xray:z"}, []string{"xraysub:y"}, false)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"xraysub:x,xray:z", "xraysub:x,xray:z", "xray:z"}
	for i, it := range items {
		if it.Dialer != want[i] {
			t.Errorf("item %d = %q, want %q", i, it.Dialer, want[i])
		}
	}
	if _, err := editDialers(entries, nil, nil, []string{"xraysub:typo"}, false); err == nil {
		t.Fatal("ref on no selected entry accepted")
	}
}
