package warp

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeCF mimics the two client-API calls Register makes.
func fakeCF(t *testing.T, gotLicense *string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/reg":
			var req map[string]any
			if err := json.Unmarshal(body, &req); err != nil || req["key"] == "" || req["tos"] == "" {
				http.Error(w, "bad reg", 400)
				return
			}
			_, _ = io.WriteString(w, `{"id":"dev1","token":"tok1","account":{"account_type":"free"},
			  "config":{"client_id":"AQID","peers":[{"public_key":"bmXOC+F1FxEMF9dyiK2H5/1SUtzH0JuVo51h2wPfgyo=",
			  "endpoint":{"v4":"162.159.192.7:0","v6":"[2606:4700:d0::a29f:c007]:0","host":"engage.cloudflareclient.com:2408"}}],
			  "interface":{"addresses":{"v4":"172.16.0.2","v6":"2606:4700:110:8a36::2"}}}}`)
		case r.Method == http.MethodPut && r.URL.Path == "/reg/dev1/account":
			if r.Header.Get("Authorization") != "Bearer tok1" {
				http.Error(w, "unauthorized", 401)
				return
			}
			var req struct{ License string }
			_ = json.Unmarshal(body, &req)
			*gotLicense = req.License
			_, _ = io.WriteString(w, `{"account_type":"limited"}`)
		default:
			http.Error(w, "unexpected "+r.Method+" "+r.URL.Path, 404)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestRegisterBuildsWireGuard(t *testing.T) {
	var lic string
	srv := fakeCF(t, &lic)
	old := API
	API = srv.URL
	t.Cleanup(func() { API = old })

	acc, err := Register(context.Background(), srv.Client(), "")
	if err != nil {
		t.Fatal(err)
	}
	wg := acc.WireGuard
	if acc.AccountType != "free" || lic != "" {
		t.Fatalf("account %q, license %q", acc.AccountType, lic)
	}
	if wg.Endpoint != "162.159.192.7:2408" {
		t.Fatalf("endpoint %q: want the v4 IP on port 2408", wg.Endpoint)
	}
	if len(wg.Reserved) != 3 || wg.Reserved[0] != 1 || wg.Reserved[1] != 2 || wg.Reserved[2] != 3 {
		t.Fatalf("reserved %v: want client_id AQID = [1 2 3]", wg.Reserved)
	}
	ob, err := wg.Outbound()
	if err != nil {
		t.Fatal(err)
	}
	s := string(ob)
	for _, want := range []string{`"protocol":"wireguard"`, `"172.16.0.2/32"`, `"2606:4700:110:8a36::2/128"`,
		`"reserved":[1,2,3]`, `"domainStrategy":"ForceIPv4v6"`, `"noKernelTun":true`} {
		if !strings.Contains(s, want) {
			t.Errorf("outbound lacks %s: %s", want, s)
		}
	}
}

func TestRegisterAppliesLicense(t *testing.T) {
	var lic string
	srv := fakeCF(t, &lic)
	old := API
	API = srv.URL
	t.Cleanup(func() { API = old })

	acc, err := Register(context.Background(), srv.Client(), " KEY-123 ")
	if err != nil {
		t.Fatal(err)
	}
	if lic != "KEY-123" || acc.AccountType != "limited" {
		t.Fatalf("license %q, account %q", lic, acc.AccountType)
	}
}

func TestRegisterSurfacesAPIError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"errors":[{"message":"too many registrations"}]}`, 429)
	}))
	t.Cleanup(srv.Close)
	old := API
	API = srv.URL
	t.Cleanup(func() { API = old })

	_, err := Register(context.Background(), srv.Client(), "")
	if err == nil || !strings.Contains(err.Error(), "too many registrations") {
		t.Fatalf("err = %v", err)
	}
}

func TestEndpointFallback(t *testing.T) {
	for in, want := range map[string]string{
		"162.159.192.9:0": "162.159.192.9:2408",
		"162.159.192.9":   "162.159.192.9:2408",
		"":                DefaultEndpoint,
		"engage.x:2408":   DefaultEndpoint,
	} {
		if got := endpoint(in); got != want {
			t.Errorf("endpoint(%q) = %q, want %q", in, got, want)
		}
	}
}
