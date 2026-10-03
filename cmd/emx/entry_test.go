package main

import "testing"

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
