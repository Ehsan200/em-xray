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
}

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
	"vmess-ws": {
		Name: "vmess-ws", Description: "vmess + websocket — CDN-friendly",
		Protocol: "vmess", Network: "ws", Security: "none", Listen: "0.0.0.0", Path: "/",
	},
	"vmess-tcp": {
		Name: "vmess-tcp", Description: "vmess over plain tcp",
		Protocol: "vmess", Network: "tcp", Security: "none", Listen: "0.0.0.0",
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
		RealityDest: t.RealityDest,
		RealitySNI:  t.RealitySNI,
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
		in.Network = "tcp"
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
	return nil
}
