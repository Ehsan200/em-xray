package xray

import (
	"fmt"
	"sort"
)

// Template is a named preset of inbound defaults. It captures the "shape" of a
// server (protocol + transport + security) so a user can create a working,
// secure inbound with zero required input.
type Template struct {
	Name        string
	Description string
	Protocol    string
	Network     string
	Security    string
	Flow        string
	Listen      string
	RealityDest string
	RealitySNI  string
	Path        string
	Host        string
	TLSSNI      string
}

// TLSCertFunc generates a self-signed TLS keypair for the given SNI. The daemon
// overrides it to shell out to `xray tls cert`; the pure-Go NewSelfSignedTLS is
// the default so core unit tests need no xray binary.
var TLSCertFunc = NewSelfSignedTLS

// DefaultTemplate is used when none is specified: vless + REALITY + vision, the
// most robust choice against active probing, and fully auto-configurable.
const DefaultTemplate = "vless-reality"

// Templates are the built-in presets. Add more here; they need no migration.
var templates = map[string]Template{
	"vless-reality": {
		Name: "vless-reality", Description: "vless + REALITY (vision) — best against censorship",
		Protocol: "vless", Network: "tcp", Security: "reality", Flow: "xtls-rprx-vision",
		Listen: "0.0.0.0", RealityDest: "www.microsoft.com:443", RealitySNI: "www.microsoft.com",
	},
	"vless-reality-grpc": {
		Name: "vless-reality-grpc", Description: "vless + REALITY over gRPC — multiplexed, probe-resistant",
		Protocol: "vless", Network: "grpc", Security: "reality", Listen: "0.0.0.0",
		RealityDest: "www.microsoft.com:443", RealitySNI: "www.microsoft.com", Path: "grpc",
	},
	"vless-reality-xhttp": {
		Name: "vless-reality-xhttp", Description: "vless + REALITY over XHTTP — CDN-splittable, probe-resistant",
		Protocol: "vless", Network: "xhttp", Security: "reality", Listen: "0.0.0.0",
		RealityDest: "www.microsoft.com:443", RealitySNI: "www.microsoft.com", Path: "/",
	},
	"vless-tls": {
		Name: "vless-tls", Description: "vless + TLS (vision, self-signed) — no domain needed",
		Protocol: "vless", Network: "tcp", Security: "tls", Flow: "xtls-rprx-vision",
		Listen: "0.0.0.0",
	},
	"vless-tls-ws": {
		Name: "vless-tls-ws", Description: "vless + TLS over websocket (self-signed) — CDN-friendly",
		Protocol: "vless", Network: "ws", Security: "tls", Listen: "0.0.0.0",
		Path: "/",
	},
	"vless-tls-xhttp": {
		Name: "vless-tls-xhttp", Description: "vless + TLS over XHTTP (self-signed)",
		Protocol: "vless", Network: "xhttp", Security: "tls", Listen: "0.0.0.0",
		Path: "/",
	},
	"vless-tls-grpc": {
		Name: "vless-tls-grpc", Description: "vless + TLS over gRPC (self-signed)",
		Protocol: "vless", Network: "grpc", Security: "tls", Listen: "0.0.0.0",
		Path: "grpc",
	},
	"vless-tls-httpupgrade": {
		Name: "vless-tls-httpupgrade", Description: "vless + TLS over HTTPUpgrade (self-signed)",
		Protocol: "vless", Network: "httpupgrade", Security: "tls", Listen: "0.0.0.0",
		Path: "/",
	},
	"vmess-ws": {
		Name: "vmess-ws", Description: "vmess + websocket — CDN-friendly",
		Protocol: "vmess", Network: "ws", Security: "none", Listen: "0.0.0.0", Path: "/",
	},
	"vmess-tcp": {
		Name: "vmess-tcp", Description: "vmess over plain tcp",
		Protocol: "vmess", Network: "tcp", Security: "none", Listen: "0.0.0.0",
	},
	"vmess-tls-ws": {
		Name: "vmess-tls-ws", Description: "vmess + TLS over websocket (self-signed)",
		Protocol: "vmess", Network: "ws", Security: "tls", Listen: "0.0.0.0",
		Path: "/",
	},
	"trojan-tls": {
		Name: "trojan-tls", Description: "trojan + TLS (self-signed) — no domain needed",
		Protocol: "trojan", Network: "tcp", Security: "tls", Listen: "0.0.0.0",
	},
	"trojan-tls-ws": {
		Name: "trojan-tls-ws", Description: "trojan + TLS over websocket (self-signed)",
		Protocol: "trojan", Network: "ws", Security: "tls", Listen: "0.0.0.0",
		Path: "/",
	},
	"hysteria2": {
		Name: "hysteria2", Description: "hysteria2 over QUIC + TLS (self-signed) — UDP, fast on lossy links",
		Protocol: "hysteria", Network: "hysteria", Security: "tls", Listen: "0.0.0.0",
	},
	"socks": {
		Name: "socks", Description: "local SOCKS5 proxy (loopback)",
		Protocol: "socks", Network: "tcp", Security: "none", Listen: "127.0.0.1",
	},
}

// TemplateByName returns a built-in template.
func TemplateByName(name string) (Template, error) {
	if name == "" {
		name = DefaultTemplate
	}
	t, ok := templates[name]
	if !ok {
		return Template{}, fmt.Errorf("unknown template %q (see: emx template ls)", name)
	}
	return t, nil
}

// TemplateList returns all templates sorted by name.
func TemplateList() []Template {
	out := make([]Template, 0, len(templates))
	for _, t := range templates {
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// NewInboundFromTemplate builds an inbound from a template + target, generating
// all credentials/keys and filling secure defaults. Port stays 0 (the store
// assigns it) and PublicHost stays blank (set later or auto-detected).
func NewInboundFromTemplate(name, templateName, target string) (*Inbound, error) {
	n, err := ValidName(name)
	if err != nil {
		return nil, err
	}
	if _, _, err := ParseTarget(target); err != nil {
		return nil, err
	}
	t, err := TemplateByName(templateName)
	if err != nil {
		return nil, err
	}
	in := &Inbound{
		Name:        n,
		Protocol:    t.Protocol,
		Network:     t.Network,
		Security:    t.Security,
		Flow:        t.Flow,
		Listen:      t.Listen,
		Path:        t.Path,
		Host:        t.Host,
		RealityDest: t.RealityDest,
		RealitySNI:  t.RealitySNI,
		TLSSNI:      t.TLSSNI,
		Enabled:     true,
		Target:      target,
	}
	if err := Materialize(in); err != nil {
		return nil, err
	}
	return in, nil
}

// Materialize fills any blank required fields of an inbound with generated
// credentials/keys and per-protocol defaults. Idempotent: existing values are
// preserved, so it is safe to call after user overrides.
func Materialize(in *Inbound) error {
	if in.Network == "" {
		if in.Protocol == "hysteria" {
			in.Network = "hysteria" // hysteria protocol requires its own transport
		} else {
			in.Network = "tcp"
		}
	}
	if in.Listen == "" {
		if in.Protocol == "socks" {
			in.Listen = "127.0.0.1"
		} else {
			in.Listen = "0.0.0.0"
		}
	}
	switch in.Protocol {
	case "vless", "vmess":
		if in.UUID == "" {
			u, err := NewUUID()
			if err != nil {
				return err
			}
			in.UUID = u
		}
	case "trojan":
		if in.Password == "" {
			p, err := NewPassword()
			if err != nil {
				return err
			}
			in.Password = p
		}
	case "hysteria":
		if in.HysteriaAuth == "" {
			a, err := NewPassword()
			if err != nil {
				return err
			}
			in.HysteriaAuth = a
		}
	}
	if in.Security == "reality" {
		if in.RealityPrivateKey == "" {
			k, err := NewRealityKeys()
			if err != nil {
				return err
			}
			in.RealityPrivateKey, in.RealityPublicKey = k.PrivateKey, k.PublicKey
		}
		if in.RealityShortID == "" {
			sid, err := NewShortID(8)
			if err != nil {
				return err
			}
			in.RealityShortID = sid
		}
		if in.RealityDest == "" {
			in.RealityDest = "www.microsoft.com:443"
		}
		if in.RealitySNI == "" {
			in.RealitySNI = "www.microsoft.com"
		}
	}
	if in.Security == "tls" {
		// SNI is OPTIONAL for self-signed TLS — clients trust the cert via
		// allowInsecure and can dial by IP with no serverName. Only set when the
		// user actually has a domain.
		if in.TLSCert == "" || in.TLSKey == "" {
			c, err := TLSCertFunc(in.TLSSNI)
			if err != nil {
				return err
			}
			in.TLSCert, in.TLSKey = c.Certificate, c.PrivateKey
		}
	}
	return nil
}
