package xray

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
)

// ShareLink builds the client-facing share link (vless://, vmess://, trojan://)
// for an inbound so a user can import it into their client. host overrides
// in.PublicHost; if both are empty a "SERVER_IP" placeholder is used. Returns ""
// for protocols with no share-link form (socks).
func ShareLink(in Inbound, host string) string {
	if host == "" {
		host = in.PublicHost
	}
	if host == "" {
		host = "SERVER_IP"
	}
	port := strconv.Itoa(in.Port)

	switch in.Protocol {
	case "vless":
		q := url.Values{}
		q.Set("encryption", "none")
		q.Set("type", orDefault(in.Network, "tcp"))
		if in.Flow != "" {
			q.Set("flow", in.Flow)
		}
		switch in.Security {
		case "reality":
			q.Set("security", "reality")
			q.Set("sni", in.RealitySNI)
			q.Set("fp", "chrome")
			q.Set("pbk", in.RealityPublicKey)
			q.Set("sid", in.RealityShortID)
		case "tls":
			q.Set("security", "tls")
		}
		if in.Network == "ws" {
			q.Set("path", orDefault(in.Path, "/"))
			if in.Host != "" {
				q.Set("host", in.Host)
			}
		}
		return fmt.Sprintf("vless://%s@%s:%s?%s#%s", in.UUID, host, port, q.Encode(), url.QueryEscape(in.Name))

	case "vmess":
		tls := ""
		if in.Security == "tls" {
			tls = "tls"
		}
		vm := map[string]any{
			"v": "2", "ps": in.Name, "add": host, "port": port, "id": in.UUID,
			"aid": "0", "scy": "auto", "net": orDefault(in.Network, "tcp"),
			"type": "none", "host": in.Host, "path": orDefault(in.Path, ""), "tls": tls,
		}
		raw, _ := json.Marshal(vm)
		return "vmess://" + base64.StdEncoding.EncodeToString(raw)

	case "trojan":
		q := url.Values{}
		if in.Security == "tls" {
			q.Set("security", "tls")
		}
		q.Set("type", orDefault(in.Network, "tcp"))
		return fmt.Sprintf("trojan://%s@%s:%s?%s#%s", in.Password, host, port, q.Encode(), url.QueryEscape(in.Name))

	default:
		return ""
	}
}
