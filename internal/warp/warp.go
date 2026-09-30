// Package warp registers a Cloudflare WARP device and turns it into a
// WireGuard entry, so an inbound routed to it exits the internet from a
// Cloudflare IP. It talks to the same client API the official apps (and wgcf)
// use; no Cloudflare account is needed for the free tier.
package warp

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/ehsan200/em-xray/core/xray"
)

// API is the client API base. A var so tests can point it at a fake.
var API = "https://api.cloudflareclient.com/v0a2158"

// DefaultEndpoint is WARP's anycast WireGuard endpoint. Used by IP, not by
// engage.cloudflareclient.com, so the tunnel never depends on the box's DNS.
const DefaultEndpoint = "162.159.192.1:2408"

// Account is a registered WARP device: what the entry needs (WireGuard) plus
// the id/token that authorize later changes to it (a WARP+ license).
type Account struct {
	ID          string
	Token       string
	AccountType string // "free", "limited" (WARP+), …
	WireGuard   xray.WireGuard
}

type regReply struct {
	ID      string `json:"id"`
	Token   string `json:"token"`
	Account struct {
		AccountType string `json:"account_type"`
	} `json:"account"`
	Config struct {
		ClientID string `json:"client_id"`
		Peers    []struct {
			PublicKey string `json:"public_key"`
			Endpoint  struct {
				V4   string `json:"v4"`
				Host string `json:"host"`
			} `json:"endpoint"`
		} `json:"peers"`
		Interface struct {
			Addresses struct {
				V4 string `json:"v4"`
				V6 string `json:"v6"`
			} `json:"addresses"`
		} `json:"interface"`
	} `json:"config"`
}

// Register creates a new WARP device with a fresh key. license, when set, is
// a WARP+ key applied right after (a license can be bound to a few devices at
// once; Cloudflare refuses it past that).
func Register(ctx context.Context, hc *http.Client, license string) (*Account, error) {
	priv, pub, err := xray.NewWireGuardKey()
	if err != nil {
		return nil, err
	}
	body := map[string]any{
		"key":           pub,
		"install_id":    "",
		"fcm_token":     "",
		"tos":           time.Now().UTC().Format("2006-01-02T15:04:05.000Z"),
		"model":         "PC",
		"serial_number": "",
		"locale":        "en_US",
	}
	var reg regReply
	if err := call(ctx, hc, http.MethodPost, API+"/reg", "", body, &reg); err != nil {
		return nil, fmt.Errorf("register WARP device: %w", err)
	}
	if reg.ID == "" || reg.Token == "" || len(reg.Config.Peers) == 0 || reg.Config.Peers[0].PublicKey == "" {
		return nil, fmt.Errorf("register WARP device: incomplete reply from Cloudflare")
	}
	acc := &Account{ID: reg.ID, Token: reg.Token, AccountType: reg.Account.AccountType}

	var addrs []string
	if a := reg.Config.Interface.Addresses.V4; a != "" {
		addrs = append(addrs, a)
	}
	if a := reg.Config.Interface.Addresses.V6; a != "" {
		addrs = append(addrs, a)
	}
	var reserved []int
	if id, err := base64.StdEncoding.DecodeString(reg.Config.ClientID); err == nil && len(id) == 3 {
		reserved = []int{int(id[0]), int(id[1]), int(id[2])}
	}
	acc.WireGuard = xray.WireGuard{
		PrivateKey:    priv,
		Addresses:     addrs,
		PeerPublicKey: reg.Config.Peers[0].PublicKey,
		Endpoint:      endpoint(reg.Config.Peers[0].Endpoint.V4),
		Reserved:      reserved,
		MTU:           xray.DefaultWireGuardMTU,
	}
	if err := acc.WireGuard.Validate(); err != nil {
		return nil, fmt.Errorf("register WARP device: %w", err)
	}

	if license = strings.TrimSpace(license); license != "" {
		var out struct {
			AccountType string `json:"account_type"`
		}
		err := call(ctx, hc, http.MethodPut, API+"/reg/"+reg.ID+"/account", reg.Token,
			map[string]any{"license": license}, &out)
		if err != nil {
			return nil, fmt.Errorf("apply WARP+ license: %w", err)
		}
		acc.AccountType = out.AccountType
	}
	return acc, nil
}

// endpoint takes the peer's v4 address from the reply (Cloudflare sends it
// with port 0) and puts it on WARP's WireGuard port, falling back to the
// well-known anycast address.
func endpoint(v4 string) string {
	host := v4
	if h, _, err := net.SplitHostPort(v4); err == nil {
		host = h
	}
	if net.ParseIP(host) == nil {
		return DefaultEndpoint
	}
	return net.JoinHostPort(host, "2408")
}

func call(ctx context.Context, hc *http.Client, method, url, token string, in, out any) error {
	b, err := json.Marshal(in)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, url, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json; charset=UTF-8")
	req.Header.Set("User-Agent", "okhttp/3.12.1")
	req.Header.Set("CF-Client-Version", "a-6.30-3596")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("bad reply: %w", err)
	}
	return nil
}
