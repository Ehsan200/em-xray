package xray

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"net/netip"
	"strconv"
	"strings"

	"golang.org/x/crypto/curve25519"
)

// WireGuard is one WireGuard tunnel as an entry needs it: this box's key and
// tunnel addresses, and the one peer it dials. Cloudflare WARP (internal/warp)
// is built from it, so a WARP account becomes an ordinary entry whose exit IP
// is Cloudflare's.
type WireGuard struct {
	PrivateKey    string   // base64, this box
	Addresses     []string // tunnel addresses, CIDR (a bare IP gets /32 or /128)
	PeerPublicKey string   // base64
	PresharedKey  string   // base64, optional
	Endpoint      string   // host:port of the peer
	Reserved      []int    // 3 bytes; WARP's client_id, empty elsewhere
	MTU           int      // 0 => 1280
	KeepAlive     int      // seconds, 0 => off
}

// DefaultWireGuardMTU fits WireGuard inside a 1280-byte IPv6 minimum path with
// room to spare; WARP and most providers ship it.
const DefaultWireGuardMTU = 1280

// NewWireGuardKey returns a fresh clamped X25519 keypair, standard base64 as
// WireGuard writes it.
func NewWireGuardKey() (priv, pub string, err error) {
	var k [32]byte
	if _, err := rand.Read(k[:]); err != nil {
		return "", "", err
	}
	k[0] &= 248
	k[31] &= 127
	k[31] |= 64
	p, err := curve25519.X25519(k[:], curve25519.Basepoint)
	if err != nil {
		return "", "", err
	}
	return base64.StdEncoding.EncodeToString(k[:]), base64.StdEncoding.EncodeToString(p), nil
}

// Validate checks the fields xray would otherwise reject at start-up, so a bad
// config is refused when it is added instead of when xray restarts.
func (w WireGuard) Validate() error {
	if err := checkWGKey("private key", w.PrivateKey); err != nil {
		return err
	}
	if err := checkWGKey("peer public key", w.PeerPublicKey); err != nil {
		return err
	}
	if w.PresharedKey != "" {
		if err := checkWGKey("preshared key", w.PresharedKey); err != nil {
			return err
		}
	}
	if len(w.Addresses) == 0 {
		return fmt.Errorf("wireguard: no tunnel address")
	}
	for _, a := range w.Addresses {
		if _, err := wgPrefix(a); err != nil {
			return err
		}
	}
	host, port, err := net.SplitHostPort(w.Endpoint)
	if err != nil || host == "" {
		return fmt.Errorf("wireguard: bad endpoint %q (want host:port)", w.Endpoint)
	}
	if p, err := strconv.Atoi(port); err != nil || p < 1 || p > 65535 {
		return fmt.Errorf("wireguard: bad endpoint port in %q", w.Endpoint)
	}
	if len(w.Reserved) != 0 && len(w.Reserved) != 3 {
		return fmt.Errorf("wireguard: reserved must be 3 bytes, got %d", len(w.Reserved))
	}
	for _, b := range w.Reserved {
		if b < 0 || b > 255 {
			return fmt.Errorf("wireguard: reserved byte %d out of range", b)
		}
	}
	return nil
}

func checkWGKey(what, k string) error {
	b, err := base64.StdEncoding.DecodeString(strings.TrimSpace(k))
	if err != nil || len(b) != 32 {
		return fmt.Errorf("wireguard: %s is not a 32-byte base64 key", what)
	}
	return nil
}

// wgPrefix reads a tunnel address, giving a bare IP its host-length prefix.
func wgPrefix(a string) (netip.Prefix, error) {
	a = strings.TrimSpace(a)
	if p, err := netip.ParsePrefix(a); err == nil {
		return p, nil
	}
	ip, err := netip.ParseAddr(a)
	if err != nil {
		return netip.Prefix{}, fmt.Errorf("wireguard: bad address %q", a)
	}
	return netip.PrefixFrom(ip, ip.BitLen()), nil
}

// Outbound builds the xray wireguard outbound (no tag).
//
// The tunnel runs in xray's userspace stack (noKernelTun): a kernel TUN would
// need root and could install routes on the box itself. domainStrategy follows
// the tunnel's address families, because the peer forwards only the families
// this box has an address in — ForceIP on a v4-only tunnel would send every
// AAAA-first destination into a black hole.
func (w WireGuard) Outbound() (json.RawMessage, error) {
	if err := w.Validate(); err != nil {
		return nil, err
	}
	var addrs []any
	v4, v6 := false, false
	for _, a := range w.Addresses {
		p, _ := wgPrefix(a)
		addrs = append(addrs, p.String())
		if p.Addr().Is4() {
			v4 = true
		} else {
			v6 = true
		}
	}
	strategy := "ForceIPv4v6"
	switch {
	case v4 && !v6:
		strategy = "ForceIPv4"
	case v6 && !v4:
		strategy = "ForceIPv6"
	}
	mtu := w.MTU
	if mtu <= 0 {
		mtu = DefaultWireGuardMTU
	}
	peer := map[string]any{
		"publicKey":  strings.TrimSpace(w.PeerPublicKey),
		"endpoint":   w.Endpoint,
		"allowedIPs": []any{"0.0.0.0/0", "::/0"},
	}
	if w.PresharedKey != "" {
		peer["preSharedKey"] = strings.TrimSpace(w.PresharedKey)
	}
	if w.KeepAlive > 0 {
		peer["keepAlive"] = w.KeepAlive
	}
	settings := map[string]any{
		"secretKey":      strings.TrimSpace(w.PrivateKey),
		"address":        addrs,
		"peers":          []any{peer},
		"mtu":            mtu,
		"noKernelTun":    true,
		"domainStrategy": strategy,
	}
	if len(w.Reserved) == 3 {
		settings["reserved"] = []any{w.Reserved[0], w.Reserved[1], w.Reserved[2]}
	}
	return outbound("wireguard", settings, nil)
}
