package xray

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
)

// ShareLinkForUser builds the share link for one additional user of an inbound,
// substituting that user's credential and labelling it "<inbound>-<user>".
func ShareLinkForUser(in Inbound, host string, u InboundUser) string {
	in.UUID = u.UUID
	in.Password = u.Password
	in.HysteriaAuth = u.Auth
	in.Name = in.Name + "-" + u.Name
	return ShareLink(in, host)
}

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
			q.Set("fp", "chrome")
			if in.TLSSNI != "" {
				q.Set("sni", in.TLSSNI)
			}
			q.Set("allowInsecure", "1") // self-signed cert
		}
		setTransportQuery(q, in)
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
			q.Set("fp", "chrome")
			if in.TLSSNI != "" {
				q.Set("sni", in.TLSSNI)
			}
			q.Set("allowInsecure", "1") // self-signed cert
		}
		q.Set("type", orDefault(in.Network, "tcp"))
		setTransportQuery(q, in)
		return fmt.Sprintf("trojan://%s@%s:%s?%s#%s", in.Password, host, port, q.Encode(), url.QueryEscape(in.Name))

	case "hysteria":
		// hysteria2:// URI (sing-box / v2rayN / official client). Auth is the
		// userinfo; insecure=1 because the server cert is self-signed.
		q := url.Values{}
		q.Set("insecure", "1")
		if in.TLSSNI != "" {
			q.Set("sni", in.TLSSNI)
		}
		return fmt.Sprintf("hysteria2://%s@%s:%s/?%s#%s", in.HysteriaAuth, host, port, q.Encode(), url.QueryEscape(in.Name))

	case "socks":
		// v2rayN-style socks URI: userinfo is base64(user:pass); omitted when the
		// inbound is no-auth. Importable by xray/v2ray/sing-box clients.
		if in.SocksUser != "" {
			creds := base64.StdEncoding.EncodeToString([]byte(in.SocksUser + ":" + in.Password))
			return fmt.Sprintf("socks://%s@%s:%s#%s", creds, host, port, url.QueryEscape(in.Name))
		}
		return fmt.Sprintf("socks://%s:%s#%s", host, port, url.QueryEscape(in.Name))

	default:
		return ""
	}
}

// TelegramSocksLink builds a Telegram proxy deeplink (tg://socks) for a socks
// inbound so it can be tapped into Telegram's proxy settings. host overrides
// in.PublicHost (same resolution as ShareLink). Returns "" for non-socks
// inbounds. User/pass are included only when the inbound has auth.
func TelegramSocksLink(in Inbound, host string) string {
	if in.Protocol != "socks" {
		return ""
	}
	if host == "" {
		host = in.PublicHost
	}
	if host == "" {
		host = "SERVER_IP"
	}
	q := url.Values{}
	q.Set("server", host)
	q.Set("port", strconv.Itoa(in.Port))
	if in.SocksUser != "" {
		q.Set("user", in.SocksUser)
		q.Set("pass", in.Password)
	}
	return "tg://socks?" + q.Encode()
}

// setTransportQuery adds transport-specific params (path/host/serviceName) to a
// vless/trojan share link based on the inbound's network.
func setTransportQuery(q url.Values, in Inbound) {
	switch in.Network {
	case "ws", "httpupgrade", "xhttp":
		q.Set("path", orDefault(in.Path, "/"))
		if in.Host != "" {
			q.Set("host", in.Host)
		}
	case "grpc":
		q.Set("serviceName", in.Path)
	}
}
