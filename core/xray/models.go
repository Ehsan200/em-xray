// Package xray holds the OS-agnostic core: data model + store, link parsing,
// config generation and the dialer logic. It has no OS coupling and ports
// unchanged across platforms.
package xray

import (
	"fmt"
	"regexp"
	"strings"
	"time"
)

// Defaults.
const (
	DefaultSubUserAgent   = "v2rayN/6.23"
	DefaultSubIntervalSec = 12 * 3600 // 12h
	DefaultSubNodeCap     = 30
)

// Subscription is a remote URL that yields a volatile pool of nodes. It is never
// itself a routing target — its nodes are consumed only inside a master's Dialer.
type Subscription struct {
	ID          uint   `gorm:"primaryKey"`
	Name        string `gorm:"uniqueIndex;not null"`
	URL         string `gorm:"not null"`
	UserAgent   string // many providers gate on a v2rayN-ish UA
	IntervalSec int    // refresh cadence; 0 => default
	NodeCap     int    // max active nodes; 0 => default
	Enabled     bool   `gorm:"default:true"`
	LastFetched time.Time
	LastError   string

	// Quota, parsed from the Subscription-Userinfo header. Persisted only when
	// the header is present so a header-less fetch keeps the last figures.
	Upload   int64
	Download int64
	Total    int64
	Expire   int64 // unix seconds

	CreatedAt time.Time
	UpdatedAt time.Time
}

// EffectiveInterval applies the default when IntervalSec is unset.
func (s Subscription) EffectiveInterval() time.Duration {
	if s.IntervalSec <= 0 {
		return time.Duration(DefaultSubIntervalSec) * time.Second
	}
	return time.Duration(s.IntervalSec) * time.Second
}

// EffectiveCap applies the default when NodeCap is unset.
func (s Subscription) EffectiveCap() int {
	if s.NodeCap <= 0 {
		return DefaultSubNodeCap
	}
	return s.NodeCap
}

// SubNode is one node parsed from a subscription. VOLATILE: the whole set for a
// subscription is replaced on every fetch (ReplaceNodes). Identity is the
// content Fingerprint, not the row id or display name.
type SubNode struct {
	ID            uint `gorm:"primaryKey"` // ascending => insertion order
	SubID         uint `gorm:"index;not null"`
	Name          string
	Fingerprint   string `gorm:"index;not null"` // sha256(outbound sans tag), 16 hex
	Outbound      string `gorm:"not null"`       // raw outbound JSON
	Active        bool   // DERIVED by recomputeActive — never set directly
	LastLatencyMs int
}

// SubNodeOverride is a durable manual node-disable. It OUTLIVES the volatile
// SubNode rows, keyed by fingerprint, so a disable survives a refresh, a daemon
// restart, or the node changing position.
type SubNodeOverride struct {
	ID          uint   `gorm:"primaryKey"`
	SubID       uint   `gorm:"uniqueIndex:idx_sub_fp;not null"`
	Fingerprint string `gorm:"uniqueIndex:idx_sub_fp;not null"`
	Disabled    bool
}

// XrayEntry is a user outbound. An empty Dialer is a normal entry; a non-empty
// Dialer makes it a master whose transport tunnels through a node pool.
// XrayEntry is purely an outbound (a remote server this box dials). Traffic is
// fed to it by an Inbound (see Inbound.Target). A non-empty Dialer makes it a
// master whose transport tunnels through a node pool.
type XrayEntry struct {
	ID       uint   `gorm:"primaryKey"`
	Name     string `gorm:"uniqueIndex;not null"`
	Outbound string `gorm:"not null"` // raw outbound JSON
	Enabled  bool   `gorm:"default:true"`
	Dialer   string // "" = normal; else comma-sep typed refs: xray:N,xraysub:N,proxy:N
	// Mux opts the entry's outbound into xray mux.cool where MuxSupport allows
	// (see mux.go); ignored elsewhere.
	Mux       bool
	CreatedAt time.Time
	UpdatedAt time.Time
}

// IsMaster reports whether the entry has a dialer pool.
func (e XrayEntry) IsMaster() bool { return strings.TrimSpace(e.Dialer) != "" }

// Inbound is a server listener this box exposes (vless/vmess/socks/trojan) whose
// traffic egresses through Target. This is the entry point users connect to —
// e.g. a vless-reality server routed to a master so its traffic exits via the
// fastest node. Credentials + keys are auto-generated; blanks get secure
// defaults from the chosen template.
type Inbound struct {
	ID       uint   `gorm:"primaryKey"`
	Name     string `gorm:"uniqueIndex;not null"`
	Protocol string `gorm:"not null"` // vless|vmess|socks|trojan|hysteria
	Listen   string // default per protocol (0.0.0.0 for servers, 127.0.0.1 for socks)
	Port     int    // 0 => auto-assign, except Unix-socket listeners (no TCP port)
	Enabled  bool   `gorm:"default:true"`

	// Credential (auto-generated): UUID for vless/vmess, Password for trojan,
	// HysteriaAuth for hysteria. For a public socks inbound SocksUser+Password are
	// the username/password pair (blank SocksUser => no-auth socks).
	UUID         string
	Password     string
	HysteriaAuth string // hysteria2 auth string (primary client)
	SocksUser    string // socks5 username (public socks; pairs with Password)
	Flow         string // vless flow, e.g. xtls-rprx-vision

	// Transport + security.
	Network     string // tcp|ws|grpc|xhttp (default tcp)
	Path        string // ws/xhttp path
	Host        string // ws Host header / xhttp host
	Security    string // reality|tls|none
	XHTTPMode   string // auto|packet-up|stream-up|stream-one (xhttp only)
	ClientEmail string // primary client's xray stats identity; blank => inbound name

	// Reality params (auto-generated when Security=reality).
	RealityPrivateKey string
	RealityPublicKey  string
	RealityShortID    string
	RealityDest       string // steal-oneself target, e.g. www.microsoft.com:443
	RealitySNI        string // serverName advertised, e.g. www.microsoft.com

	// TLS params (auto-generated when Security=tls). Self-signed keypair — the
	// server has no CA cert, so clients trust it via allowInsecure. Generated by
	// `xray tls cert` (see TLSCertFunc).
	TLSCert string // PEM certificate (public)
	TLSKey  string // PEM private key
	TLSSNI  string // serverName advertised

	// PublicHost is the address clients dial (for the share link); the listen
	// port is Port. Auto-detected/overridable; may be a placeholder.
	PublicHost string

	// Target egress: "master:NAME" | "xray:NAME" | "direct".
	Target string `gorm:"not null"`

	CreatedAt time.Time
	UpdatedAt time.Time

	// Users are extra clients sharing this listener (multi-tenant). Not a DB
	// column — the daemon loads them from InboundUser and populates this before
	// config generation. The inbound's own UUID/Password is the primary client.
	Users []InboundUser `gorm:"-" json:"Users,omitempty"`
}

// InboundUser is an additional client on an inbound: its own credential, a stable
// stats email, and an optional byte cap. When lifetime usage reaches ByteCap the
// daemon stops emitting the client (an over-quota user is denied access).
type InboundUser struct {
	ID        uint   `gorm:"primaryKey"`
	InboundID uint   `gorm:"index:idx_user_inbound,unique,priority:1;not null"`
	Name      string `gorm:"index:idx_user_inbound,unique,priority:2;not null"`
	UUID      string // vless/vmess
	Password  string // trojan
	Auth      string // hysteria2
	Email     string `gorm:"index;not null"` // stats tag: user>>>EMAIL>>>traffic>>>...
	ByteCap   int64  // 0 = unlimited; else deny once up+down reaches it
	Enabled   bool   `gorm:"default:true"`
	CreatedAt time.Time
	UpdatedAt time.Time
}

// Target kinds for an inbound's egress.
const (
	TargetDirect = "direct"
	KindMaster   = "master"
	KindXray     = "xray"
)

// ParseTarget splits an inbound Target into (kind, name). "direct" yields
// (direct, ""). Unknown forms return an error.
func ParseTarget(t string) (kind, name string, err error) {
	t = strings.TrimSpace(t)
	if t == "" || t == TargetDirect {
		return TargetDirect, "", nil
	}
	k, n, ok := strings.Cut(t, ":")
	if !ok || n == "" {
		return "", "", fmt.Errorf("bad target %q (want master:NAME | xray:NAME | direct)", t)
	}
	switch k {
	case KindMaster, KindXray:
		return k, n, nil
	default:
		return "", "", fmt.Errorf("unknown target kind %q", k)
	}
}

// CanonicalTarget normalizes a Target to its stored form, turning the blank
// spelling of "direct" into the literal word. Blank and "direct" route
// identically, but only one of them is legible: an inbound that egresses from
// this box's own IP must say so on its row, in `in ls`, and in its config JSON,
// so it is never mistaken for a tunnelled one.
func CanonicalTarget(t string) (string, error) {
	kind, name, err := ParseTarget(t)
	if err != nil {
		return "", err
	}
	if kind == TargetDirect {
		return TargetDirect, nil
	}
	return kind + ":" + name, nil
}

var nameRe = regexp.MustCompile(`^[A-Za-z0-9 ._-]+$`)

// NormalizeName trims and collapses internal whitespace.
func NormalizeName(s string) string {
	return strings.Join(strings.Fields(strings.TrimSpace(s)), " ")
}

// ValidName normalizes then validates a subscription/entry name. Returns the
// normalized form or an error.
func ValidName(s string) (string, error) {
	n := NormalizeName(s)
	if n == "" {
		return "", fmt.Errorf("name is empty")
	}
	if len(n) > 64 {
		return "", fmt.Errorf("name too long (%d > 64)", len(n))
	}
	if !nameRe.MatchString(n) {
		return "", fmt.Errorf("name has invalid characters: %q", n)
	}
	return n, nil
}
