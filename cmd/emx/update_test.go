package main

import "testing"

// TestLooksLikeAddress guards the fork in --proxy: an address is used as-is, a
// name is resolved against the daemon's inbounds. Misjudging either way turns a
// clear error into a confusing one.
func TestLooksLikeAddress(t *testing.T) {
	addresses := []string{
		"127.0.0.1:1080",
		"10.0.0.1:8080",
		"proxy.internal:3128",
		"[::1]:1080",
		"socks5h://127.0.0.1:1080",
		"http://user:pw@10.0.0.1:8080",
	}
	for _, s := range addresses {
		if !looksLikeAddress(s) {
			t.Errorf("looksLikeAddress(%q) = false, want true", s)
		}
	}

	names := []string{
		"tg",
		"gate",
		"eu-backup",
		"eu:backup", // colon, but no numeric port → still a name
		"host:http", // named port is not a port we can dial as an int
		"127.0.0.1", // bare host, no port
	}
	for _, s := range names {
		if looksLikeAddress(s) {
			t.Errorf("looksLikeAddress(%q) = true, want false", s)
		}
	}
}

func TestSameVersion(t *testing.T) {
	for _, c := range []struct {
		got, want string
		ok        bool
	}{
		{"v1.2.3", "v1.2.3", true},
		{"1.2.3", "v1.2.3", true},
		{"v1.2.2", "v1.2.3", false},
		{"", "v1.2.3", false},
		{"anything", "dev", true},
	} {
		if got := sameVersion(c.got, c.want); got != c.ok {
			t.Errorf("sameVersion(%q, %q) = %v, want %v", c.got, c.want, got, c.ok)
		}
	}
}
