package xray

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

// ParsedLink is one share link turned into an xray outbound. Name is the
// human label from the URL fragment; Outbound is the outbound JSON without a
// "tag" (the caller assigns tags at config-generation time).
type ParsedLink struct {
	Name     string
	Outbound json.RawMessage
}

// ParseLink parses a vless/vmess/trojan/ss/hysteria2 share link into an xray outbound.
func ParseLink(link string) (*ParsedLink, error) {
	link = strings.TrimSpace(link)
	scheme, _, ok := strings.Cut(link, "://")
	if !ok {
		return nil, fmt.Errorf("not a share link: %q", trunc(link))
	}
	switch strings.ToLower(scheme) {
	case "vless":
		return parseVLESS(link)
	case "vmess":
		return parseVMess(link)
	case "trojan":
		return parseTrojan(link)
	case "ss":
		return parseSS(link)
	case "hysteria2", "hy2":
		return parseHysteria2(link)
	default:
		return nil, fmt.Errorf("unsupported scheme %q", scheme)
	}
}

// outbound assembles the canonical outbound object and marshals it.
func outbound(protocol string, settings, stream map[string]any) (json.RawMessage, error) {
	o := map[string]any{"protocol": protocol, "settings": settings}
	if len(stream) > 0 {
		o["streamSettings"] = stream
	}
	return json.Marshal(o)
}

// ---- vless -----------------------------------------------------------------

func parseVLESS(link string) (*ParsedLink, error) {
	u, err := url.Parse(link)
	if err != nil {
		return nil, err
	}
	host, port, err := hostPort(u)
	if err != nil {
		return nil, err
	}
	id := u.User.Username()
	if id == "" {
		return nil, fmt.Errorf("vless: missing uuid")
	}
	q := u.Query()

	user := map[string]any{"id": id, "encryption": orDefault(q.Get("encryption"), "none")}
	if flow := q.Get("flow"); flow != "" {
		user["flow"] = flow
	}
	settings := map[string]any{"vnext": []any{map[string]any{
		"address": host, "port": port, "users": []any{user},
	}}}
	stream := buildStream(q)
	ob, err := outbound("vless", settings, stream)
	if err != nil {
		return nil, err
	}
	return &ParsedLink{Name: fragmentName(u, host), Outbound: ob}, nil
}

// ---- trojan ----------------------------------------------------------------

func parseTrojan(link string) (*ParsedLink, error) {
	u, err := url.Parse(link)
	if err != nil {
		return nil, err
	}
	host, port, err := hostPort(u)
	if err != nil {
		return nil, err
	}
	password := u.User.Username()
	if password == "" {
		return nil, fmt.Errorf("trojan: missing password")
	}
	q := u.Query()
	settings := map[string]any{"servers": []any{map[string]any{
		"address": host, "port": port, "password": password,
	}}}
	stream := buildStream(q)
	// trojan defaults to TLS when no security is specified.
	if _, has := stream["security"]; !has {
		stream["security"] = "tls"
		applyTLS(stream, q, host)
	}
	ob, err := outbound("trojan", settings, stream)
	if err != nil {
		return nil, err
	}
	return &ParsedLink{Name: fragmentName(u, host), Outbound: ob}, nil
}

// ---- hysteria2 -------------------------------------------------------------

// parseHysteria2 parses a hysteria2://auth@host:port/?insecure=1&sni=... link
// into an xray hysteria outbound. Auth lives in streamSettings.hysteriaSettings;
// the protocol settings carry only the server endpoint. obfs is unsupported by
// xray's hysteria transport and is ignored. hysteria2 always runs over TLS.
func parseHysteria2(link string) (*ParsedLink, error) {
	u, err := url.Parse(link)
	if err != nil {
		return nil, err
	}
	host, port, err := hostPort(u)
	if err != nil {
		return nil, err
	}
	auth := u.User.Username()
	if pw, ok := u.User.Password(); ok && pw != "" {
		auth += ":" + pw
	}
	if auth == "" {
		return nil, fmt.Errorf("hysteria2: missing auth")
	}
	q := u.Query()

	settings := map[string]any{"version": 2, "address": host, "port": port}

	tls := map[string]any{}
	if sni := q.Get("sni"); sni != "" {
		tls["serverName"] = sni
	}
	if q.Get("insecure") == "1" || strings.EqualFold(q.Get("insecure"), "true") {
		tls["allowInsecure"] = true
	}
	stream := map[string]any{
		"network":          "hysteria",
		"security":         "tls",
		"tlsSettings":      tls,
		"hysteriaSettings": map[string]any{"version": 2, "auth": auth},
	}
	ob, err := outbound("hysteria", settings, stream)
	if err != nil {
		return nil, err
	}
	return &ParsedLink{Name: fragmentName(u, host), Outbound: ob}, nil
}

// ---- vmess -----------------------------------------------------------------

// vmessJSON is the v2rayN base64-json vmess share format.
type vmessJSON struct {
	PS   string `json:"ps"`
	Add  string `json:"add"`
	Port any    `json:"port"` // may be string or number
	ID   string `json:"id"`
	Aid  any    `json:"aid"`
	Scy  string `json:"scy"`
	Net  string `json:"net"`
	Type string `json:"type"` // header type
	Host string `json:"host"`
	Path string `json:"path"`
	TLS  string `json:"tls"`
	SNI  string `json:"sni"`
	ALPN string `json:"alpn"`
	FP   string `json:"fp"`
}

func parseVMess(link string) (*ParsedLink, error) {
	raw := strings.TrimPrefix(link, "vmess://")
	dec, ok := decodeB64(raw)
	if !ok {
		return nil, fmt.Errorf("vmess: bad base64")
	}
	var v vmessJSON
	if err := json.Unmarshal(dec, &v); err != nil {
		return nil, fmt.Errorf("vmess: bad json: %w", err)
	}
	port := toInt(v.Port)
	if v.Add == "" || port == 0 || v.ID == "" {
		return nil, fmt.Errorf("vmess: missing address/port/id")
	}
	user := map[string]any{"id": v.ID, "alterId": toInt(v.Aid), "security": orDefault(v.Scy, "auto")}
	settings := map[string]any{"vnext": []any{map[string]any{
		"address": v.Add, "port": port, "users": []any{user},
	}}}

	// Rebuild stream from the flat vmess fields via the same query machinery.
	q := url.Values{}
	q.Set("type", v.Net)
	q.Set("security", v.TLS)
	q.Set("host", v.Host)
	q.Set("path", v.Path)
	q.Set("sni", v.SNI)
	q.Set("fp", v.FP)
	q.Set("alpn", v.ALPN)
	q.Set("headerType", v.Type)
	q.Set("serviceName", v.Path) // grpc carries serviceName in path for vmess links
	stream := buildStream(q)

	ob, err := outbound("vmess", settings, stream)
	if err != nil {
		return nil, err
	}
	name := v.PS
	if name == "" {
		name = v.Add
	}
	return &ParsedLink{Name: name, Outbound: ob}, nil
}

// ---- shadowsocks -----------------------------------------------------------

func parseSS(link string) (*ParsedLink, error) {
	u, err := url.Parse(link)
	if err != nil {
		return nil, err
	}
	name := ""
	if u.Fragment != "" {
		name = decodeFragment(u.Fragment)
	}

	var method, password, host string
	var port int

	if u.Host != "" && u.User != nil {
		// SIP002: ss://base64(method:password)@host:port  (userinfo may be b64 or plain)
		host, port, err = hostPort(u)
		if err != nil {
			return nil, err
		}
		userinfo := u.User.String()
		if m, p, ok := splitMethodPass(userinfo); ok {
			method, password = m, p
		} else if b, ok := decodeB64(userinfo); ok {
			if m, p, ok := splitMethodPass(string(b)); ok {
				method, password = m, p
			}
		}
	} else {
		// Legacy: ss://base64(method:password@host:port)
		raw := strings.TrimPrefix(link, "ss://")
		if i := strings.IndexByte(raw, '#'); i >= 0 {
			raw = raw[:i]
		}
		b, ok := decodeB64(raw)
		if !ok {
			return nil, fmt.Errorf("ss: bad base64")
		}
		at := strings.LastIndexByte(string(b), '@')
		if at < 0 {
			return nil, fmt.Errorf("ss: malformed")
		}
		mp, hp := string(b)[:at], string(b)[at+1:]
		m, p, ok := splitMethodPass(mp)
		if !ok {
			return nil, fmt.Errorf("ss: missing method:password")
		}
		method, password = m, p
		h, ps, ok := strings.Cut(hp, ":")
		if !ok {
			return nil, fmt.Errorf("ss: missing host:port")
		}
		host = h
		port, _ = strconv.Atoi(ps)
	}

	if method == "" || host == "" || port == 0 {
		return nil, fmt.Errorf("ss: incomplete (method/host/port)")
	}
	settings := map[string]any{"servers": []any{map[string]any{
		"address": host, "port": port, "method": method, "password": password,
	}}}
	ob, err := outbound("shadowsocks", settings, nil)
	if err != nil {
		return nil, err
	}
	if name == "" {
		name = host
	}
	return &ParsedLink{Name: name, Outbound: ob}, nil
}

// ---- stream settings (shared by vless/trojan/vmess) ------------------------

// buildStream maps share-link query params to xray streamSettings.
func buildStream(q url.Values) map[string]any {
	stream := map[string]any{}
	network := normNetwork(q.Get("type"))
	stream["network"] = network

	switch strings.ToLower(q.Get("security")) {
	case "reality":
		stream["security"] = "reality"
		stream["realitySettings"] = realitySettings(q)
	case "tls", "xtls":
		stream["security"] = "tls"
		applyTLS(stream, q, "")
	case "", "none":
		// leave unset (plain)
	default:
		stream["security"] = "tls"
		applyTLS(stream, q, "")
	}

	host := q.Get("host")
	path := orDefault(q.Get("path"), "/")
	switch network {
	case "ws":
		ws := map[string]any{"path": path}
		if host != "" {
			ws["headers"] = map[string]any{"Host": host}
		}
		stream["wsSettings"] = ws
	case "httpupgrade":
		hu := map[string]any{"path": path}
		if host != "" {
			hu["host"] = host
		}
		stream["httpupgradeSettings"] = hu
	case "grpc":
		g := map[string]any{"serviceName": q.Get("serviceName")}
		if q.Get("mode") == "multi" {
			g["multiMode"] = true
		}
		stream["grpcSettings"] = g
	case "http": // h2
		h := map[string]any{"path": path}
		if host != "" {
			h["host"] = []any{host}
		}
		stream["httpSettings"] = h
	case "xhttp":
		x := map[string]any{"path": path, "mode": orDefault(q.Get("mode"), "auto")}
		if host != "" {
			x["host"] = host
		}
		stream["xhttpSettings"] = x
	case "tcp":
		if strings.EqualFold(q.Get("headerType"), "http") {
			hdr := map[string]any{"type": "http"}
			if host != "" {
				hdr["request"] = map[string]any{"headers": map[string]any{"Host": []any{host}}}
			}
			stream["tcpSettings"] = map[string]any{"header": hdr}
		}
	}
	return stream
}

func applyTLS(stream map[string]any, q url.Values, fallbackSNI string) {
	tls := map[string]any{}
	sni := orDefault(q.Get("sni"), q.Get("peer"))
	if sni == "" {
		sni = fallbackSNI
	}
	if sni != "" {
		tls["serverName"] = sni
	}
	if fp := q.Get("fp"); fp != "" {
		tls["fingerprint"] = fp
	}
	if alpn := q.Get("alpn"); alpn != "" {
		tls["alpn"] = splitComma(alpn)
	}
	if q.Get("allowInsecure") == "1" || strings.EqualFold(q.Get("allowInsecure"), "true") {
		tls["allowInsecure"] = true
	}
	stream["tlsSettings"] = tls
}

func realitySettings(q url.Values) map[string]any {
	r := map[string]any{}
	if sni := orDefault(q.Get("sni"), q.Get("peer")); sni != "" {
		r["serverName"] = sni
	}
	if fp := q.Get("fp"); fp != "" {
		r["fingerprint"] = fp
	}
	if pbk := q.Get("pbk"); pbk != "" {
		r["publicKey"] = pbk
	}
	if sid := q.Get("sid"); sid != "" {
		r["shortId"] = sid
	}
	if spx := q.Get("spx"); spx != "" {
		r["spiderX"] = spx
	}
	return r
}

// normNetwork canonicalises transport aliases to xray network names.
func normNetwork(t string) string {
	switch strings.ToLower(strings.TrimSpace(t)) {
	case "", "tcp", "raw":
		return "tcp"
	case "ws", "websocket":
		return "ws"
	case "grpc", "gun":
		return "grpc"
	case "h2", "http":
		return "http"
	case "xhttp", "splithttp":
		return "xhttp"
	case "httpupgrade":
		return "httpupgrade"
	case "kcp", "mkcp":
		return "kcp"
	case "quic":
		return "quic"
	default:
		return "tcp"
	}
}

// ---- small helpers ---------------------------------------------------------

func hostPort(u *url.URL) (string, int, error) {
	host := u.Hostname()
	if host == "" {
		return "", 0, fmt.Errorf("missing host")
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil || port == 0 {
		return "", 0, fmt.Errorf("missing/invalid port")
	}
	return host, port, nil
}

func fragmentName(u *url.URL, fallback string) string {
	if u.Fragment != "" {
		return decodeFragment(u.Fragment)
	}
	return fallback
}

func decodeFragment(frag string) string {
	if dec, err := url.QueryUnescape(frag); err == nil {
		return dec
	}
	return frag
}

func splitMethodPass(s string) (method, pass string, ok bool) {
	m, p, ok := strings.Cut(s, ":")
	if !ok || m == "" {
		return "", "", false
	}
	return m, p, true
}

func splitComma(s string) []any {
	parts := strings.Split(s, ",")
	out := make([]any, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func orDefault(v, def string) string {
	if strings.TrimSpace(v) == "" {
		return def
	}
	return v
}

func toInt(v any) int {
	switch x := v.(type) {
	case float64:
		return int(x)
	case int:
		return x
	case string:
		n, _ := strconv.Atoi(strings.TrimSpace(x))
		return n
	default:
		return 0
	}
}

func trunc(s string) string {
	if len(s) > 40 {
		return s[:40] + "…"
	}
	return s
}
