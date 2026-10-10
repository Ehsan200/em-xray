package xray

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
)

// OutboundLink rebuilds a share link (vless/vmess/trojan/ss/hysteria2) from
// an entry's stored outbound JSON — the reverse of ParseLink — labelled name.
// It reads the outbound as it is now, so edits made after the entry was added
// are reflected. Fields a share link can't carry (sockopt, mux, …) are left
// out; an outbound with no share-link form (freedom, wireguard, …) or missing
// its server/credential is an error saying why.
func OutboundLink(outboundJSON, name string) (string, error) {
	var ob struct {
		Protocol string         `json:"protocol"`
		Settings map[string]any `json:"settings"`
		Stream   map[string]any `json:"streamSettings"`
	}
	if err := json.Unmarshal([]byte(outboundJSON), &ob); err != nil {
		return "", fmt.Errorf("bad outbound JSON: %w", err)
	}
	switch ob.Protocol {
	case "vless":
		return vlessLink(ob.Settings, ob.Stream, name)
	case "vmess":
		return vmessLink(ob.Settings, ob.Stream, name)
	case "trojan":
		return trojanLink(ob.Settings, ob.Stream, name)
	case "shadowsocks":
		return ssLink(ob.Settings, name)
	case "hysteria":
		return hysteria2Link(ob.Settings, ob.Stream, name)
	case "":
		return "", fmt.Errorf("outbound has no protocol")
	default:
		return "", fmt.Errorf("%s outbounds have no share-link form", ob.Protocol)
	}
}

func vlessLink(settings, stream map[string]any, name string) (string, error) {
	srv, user := serverAndUser(settings, "vnext")
	host, port, err := linkEndpoint(srv)
	if err != nil {
		return "", fmt.Errorf("vless: %w", err)
	}
	id := str(user["id"])
	if id == "" {
		return "", fmt.Errorf("vless: missing uuid")
	}
	q := streamQuery(stream)
	q.Set("encryption", orDefault(str(user["encryption"]), "none"))
	if flow := str(user["flow"]); flow != "" {
		q.Set("flow", flow)
	}
	return buildURL("vless", url.User(id), host, port, "", q, name), nil
}

func trojanLink(settings, stream map[string]any, name string) (string, error) {
	srv, _ := serverAndUser(settings, "servers")
	host, port, err := linkEndpoint(srv)
	if err != nil {
		return "", fmt.Errorf("trojan: %w", err)
	}
	password := str(srv["password"])
	if password == "" {
		return "", fmt.Errorf("trojan: missing password")
	}
	q := streamQuery(stream)
	if q.Get("security") == "" {
		q.Set("security", "none") // trojan links default to tls when unset
	}
	return buildURL("trojan", url.User(password), host, port, "", q, name), nil
}

// ssLink emits SIP002 with base64url(method:password) userinfo, which every
// client (and ParseLink) accepts, 2022 keys included.
func ssLink(settings map[string]any, name string) (string, error) {
	srv, _ := serverAndUser(settings, "servers")
	host, port, err := linkEndpoint(srv)
	if err != nil {
		return "", fmt.Errorf("ss: %w", err)
	}
	method := str(srv["method"])
	if method == "" {
		return "", fmt.Errorf("ss: missing method")
	}
	creds := base64.RawURLEncoding.EncodeToString([]byte(method + ":" + str(srv["password"])))
	return buildURL("ss", url.User(creds), host, port, "", nil, name), nil
}

func hysteria2Link(settings, stream map[string]any, name string) (string, error) {
	host, port, err := linkEndpoint(settings)
	if err != nil {
		return "", fmt.Errorf("hysteria2: %w", err)
	}
	auth := str(obj(stream["hysteriaSettings"])["auth"])
	if auth == "" {
		return "", fmt.Errorf("hysteria2: missing auth")
	}
	tls := obj(stream["tlsSettings"])
	q := url.Values{}
	if sni := str(tls["serverName"]); sni != "" {
		q.Set("sni", sni)
	}
	if pin := str(tls["pinnedPeerCertSha256"]); pin != "" {
		q.Set("pinSHA256", pin)
	}
	return buildURL("hysteria2", url.User(auth), host, port, "/", q, name), nil
}

func vmessLink(settings, stream map[string]any, name string) (string, error) {
	srv, user := serverAndUser(settings, "vnext")
	host, port, err := linkEndpoint(srv)
	if err != nil {
		return "", fmt.Errorf("vmess: %w", err)
	}
	id := str(user["id"])
	if id == "" {
		return "", fmt.Errorf("vmess: missing uuid")
	}
	q := streamQuery(stream)
	path := q.Get("path")
	if q.Get("type") == "grpc" {
		path = q.Get("serviceName") // vmess links carry serviceName in path
	}
	vm := map[string]any{
		"v": "2", "ps": name, "add": host, "port": strconv.Itoa(port), "id": id,
		"aid": strconv.Itoa(toInt(user["alterId"])), "scy": orDefault(str(user["security"]), "auto"),
		"net": q.Get("type"), "type": orDefault(q.Get("headerType"), "none"),
		"host": q.Get("host"), "path": path, "tls": q.Get("security"),
	}
	for _, k := range []string{"sni", "alpn", "fp", "pcs", "vcn"} {
		if v := q.Get(k); v != "" {
			vm[k] = v
		}
	}
	raw, err := json.Marshal(vm)
	if err != nil {
		return "", err
	}
	return "vmess://" + base64.StdEncoding.EncodeToString(raw), nil
}

// streamQuery maps streamSettings back to share-link query params — the
// reverse of buildStream.
func streamQuery(stream map[string]any) url.Values {
	q := url.Values{}
	network := normNetwork(str(stream["network"]))
	q.Set("type", network)

	switch str(stream["security"]) {
	case "reality":
		r := obj(stream["realitySettings"])
		q.Set("security", "reality")
		setIf(q, "sni", str(r["serverName"]))
		setIf(q, "fp", str(r["fingerprint"]))
		setIf(q, "pbk", str(r["publicKey"]))
		setIf(q, "sid", str(r["shortId"]))
		setIf(q, "spx", str(r["spiderX"]))
	case "tls":
		t := obj(stream["tlsSettings"])
		q.Set("security", "tls")
		setIf(q, "sni", str(t["serverName"]))
		setIf(q, "fp", str(t["fingerprint"]))
		setIf(q, "alpn", strings.Join(strs(t["alpn"]), ","))
		setIf(q, "pcs", str(t["pinnedPeerCertSha256"]))
		setIf(q, "vcn", str(t["verifyPeerCertByName"]))
	}

	switch network {
	case "ws":
		ws := obj(stream["wsSettings"])
		setIf(q, "path", str(ws["path"]))
		setIf(q, "host", orDefault(str(ws["host"]), str(obj(ws["headers"])["Host"])))
	case "httpupgrade":
		hu := obj(stream["httpupgradeSettings"])
		setIf(q, "path", str(hu["path"]))
		setIf(q, "host", str(hu["host"]))
	case "grpc":
		g := obj(stream["grpcSettings"])
		setIf(q, "serviceName", str(g["serviceName"]))
		if b, _ := g["multiMode"].(bool); b {
			q.Set("mode", "multi")
		}
	case "http":
		h := obj(stream["httpSettings"])
		setIf(q, "path", str(h["path"]))
		if hosts := strs(h["host"]); len(hosts) > 0 {
			q.Set("host", hosts[0])
		}
	case "xhttp":
		x := obj(stream["xhttpSettings"])
		setIf(q, "path", str(x["path"]))
		setIf(q, "host", str(x["host"]))
		setIf(q, "mode", str(x["mode"]))
	case "tcp":
		hdr := obj(obj(stream["tcpSettings"])["header"])
		if str(hdr["type"]) == "http" {
			q.Set("headerType", "http")
			if hosts := strs(obj(obj(hdr["request"])["headers"])["Host"]); len(hosts) > 0 {
				q.Set("host", hosts[0])
			}
		}
	}
	return q
}

// serverAndUser returns the first server under settings[listKey] and its first
// user. xray also accepts the flat form (address/port/id directly in
// settings); then the settings object stands in for both.
func serverAndUser(settings map[string]any, listKey string) (srv, user map[string]any) {
	srv = settings
	if list, _ := settings[listKey].([]any); len(list) > 0 {
		srv = obj(list[0])
	}
	user = srv
	if users, _ := srv["users"].([]any); len(users) > 0 {
		user = obj(users[0])
	}
	return srv, user
}

func linkEndpoint(srv map[string]any) (string, int, error) {
	host, port := str(srv["address"]), toInt(srv["port"])
	if host == "" || port == 0 {
		return "", 0, fmt.Errorf("missing address/port")
	}
	return host, port, nil
}

func buildURL(scheme string, user *url.Userinfo, host string, port int, path string, q url.Values, name string) string {
	u := url.URL{
		Scheme: scheme, User: user, Host: net.JoinHostPort(host, strconv.Itoa(port)),
		Path: path, RawQuery: q.Encode(),
	}
	return u.String() + "#" + url.QueryEscape(name)
}

func obj(v any) map[string]any {
	m, _ := v.(map[string]any)
	return m
}

// str returns v as a string; numbers (a JSON port, say) are formatted.
func str(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	default:
		return ""
	}
}

// strs returns a string or a list of strings as a slice.
func strs(v any) []string {
	switch x := v.(type) {
	case string:
		return splitCommaStrings(x)
	case []any:
		var out []string
		for _, e := range x {
			if s := str(e); s != "" {
				out = append(out, s)
			}
		}
		return out
	default:
		return nil
	}
}

func splitCommaStrings(s string) []string {
	var out []string
	for _, p := range splitComma(s) {
		out = append(out, p.(string))
	}
	return out
}

func setIf(q url.Values, k, v string) {
	if v != "" {
		q.Set(k, v)
	}
}
