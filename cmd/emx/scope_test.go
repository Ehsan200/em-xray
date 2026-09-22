package main

import "testing"

func TestUserScopeBlockedWhenRootStateExists(t *testing.T) {
	if !userScopeBlocked(1000, "", true) {
		t.Fatal("a non-root run must defer to the root instance")
	}
	if userScopeBlocked(0, "", true) {
		t.Fatal("root is the shared scope")
	}
	if userScopeBlocked(1000, "", false) {
		t.Fatal("without root state there is nothing to share")
	}
}

func TestUserScopeOverrideOptsOut(t *testing.T) {
	for _, v := range []string{"1", "true", "YES", " on "} {
		if userScopeBlocked(1000, v, true) {
			t.Fatalf("override %q must allow a private scope", v)
		}
	}
	if !userScopeBlocked(1000, "0", true) {
		t.Fatal("0 must not opt out")
	}
}
