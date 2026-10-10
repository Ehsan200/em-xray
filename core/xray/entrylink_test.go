package xray

import (
	"encoding/base64"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// TestOutboundLinkRoundTrip parses a link, rebuilds a link from the outbound
// and parses that: both outbounds must match, and the name must survive.
func TestOutboundLinkRoundTrip(t *testing.T) {
	links := map[string]string{
		"vless reality": "vless://11111111-2222-3333-4444-555555555555@1.2.3.4:443?type=tcp&security=reality&sni=www.example.com&fp=chrome&pbk=PUBKEY&sid=ab12&spx=%2F&flow=xtls-rprx-vision#my%20node",
		"vless ws tls":  "vless://uuid-1@cdn.example.com:443?type=ws&security=tls&sni=cdn.example.com&host=cdn.example.com&path=%2Fws%3Fed%3D2048&alpn=h2,http/1.1&fp=firefox#ws",
		"vless xhttp":   "vless://uuid-2@[2001:db8::1]:8443?type=xhttp&security=tls&path=%2Fx&mode=stream-one&host=h.example.com&pcs=AB:CD#v6",
		"vless grpc":    "vless://uuid-3@g.example.com:443?type=grpc&security=tls&serviceName=svc&mode=multi#grpc",
		"vless h2":      "vless://uuid-4@h.example.com:443?type=http&security=tls&path=%2Fh2&host=h.example.com#h2",
		"vless hu":      "vless://uuid-5@h.example.com:80?type=httpupgrade&path=%2Fup&host=h.example.com#hu",
		"vless tcphttp": "vless://uuid-6@h.example.com:80?type=tcp&headerType=http&host=fake.example.com#tcphttp",
		"trojan grpc":   "trojan://p%40ss@t.example.com:443?type=grpc&serviceName=tr&sni=t.example.com#trojan",
		"trojan tls":    "trojan://secret@t.example.com:443#plain-default-tls",
		"vmess ws":      "vmess://" + b64json(`{"v":"2","ps":"vm node","add":"vm.example.com","port":"443","id":"uuid-7","aid":"0","scy":"auto","net":"ws","type":"none","host":"vm.example.com","path":"/vm","tls":"tls","sni":"vm.example.com","fp":"chrome","pcs":"aabb"}`),
		"vmess grpc":    "vmess://" + b64json(`{"v":"2","ps":"vg","add":"vg.example.com","port":443,"id":"uuid-8","aid":0,"net":"grpc","path":"gsvc","tls":"tls"}`),
		"ss":            "ss://YWVzLTI1Ni1nY206cGFzcw@ss.example.com:8388#ss%20node",
		"ss 2022":       "ss://2022-blake3-aes-128-gcm:YWJjZGVmZ2hpamtsbW5vcA%3D%3D@ss.example.com:8388#ss2022",
		"hysteria2":     "hysteria2://user:pass@hy.example.com:443/?sni=hy.example.com&pinSHA256=AA:BB&insecure=1#hy",
	}
	for label, link := range links {
		t.Run(label, func(t *testing.T) {
			first, err := ParseLink(link)
			if err != nil {
				t.Fatalf("parse original: %v", err)
			}
			rebuilt, err := OutboundLink(string(first.Outbound), first.Name)
			if err != nil {
				t.Fatalf("OutboundLink: %v", err)
			}
			second, err := ParseLink(rebuilt)
			if err != nil {
				t.Fatalf("parse rebuilt %q: %v", rebuilt, err)
			}
			if second.Name != first.Name {
				t.Errorf("name: got %q, want %q", second.Name, first.Name)
			}
			if a, b := om(t, first.Outbound), om(t, second.Outbound); !reflect.DeepEqual(a, b) {
				t.Errorf("outbound changed\n link: %s\n want: %s\n  got: %s", rebuilt, first.Outbound, second.Outbound)
			}
		})
	}
}

// Flat vless (address/id directly in settings) is valid xray config, e.g. a
// hand-written --outbound.
func TestOutboundLinkFlatVLESS(t *testing.T) {
	ob := `{"protocol":"vless","settings":{"address":"f.example.com","port":443,"id":"uuid-f","encryption":"none"},
		"streamSettings":{"network":"ws","security":"tls","tlsSettings":{"serverName":"f.example.com"},"wsSettings":{"path":"/f","host":"f.example.com"}}}`
	link, err := OutboundLink(ob, "flat")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"vless://uuid-f@f.example.com:443?", "type=ws", "security=tls", "path=%2Ff", "host=f.example.com", "#flat"} {
		if !strings.Contains(link, want) {
			t.Errorf("link %q lacks %q", link, want)
		}
	}
}

func TestOutboundLinkErrors(t *testing.T) {
	cases := map[string]string{
		"freedom":      `{"protocol":"freedom"}`,
		"wireguard":    `{"protocol":"wireguard","settings":{}}`,
		"no protocol":  `{}`,
		"bad json":     `{`,
		"vless no id":  `{"protocol":"vless","settings":{"vnext":[{"address":"a","port":1,"users":[{}]}]}}`,
		"trojan no ep": `{"protocol":"trojan","settings":{"servers":[{"password":"x"}]}}`,
	}
	for label, ob := range cases {
		if link, err := OutboundLink(ob, "x"); err == nil {
			t.Errorf("%s: want error, got %q", label, link)
		}
	}
}

func b64json(s string) string {
	if !json.Valid([]byte(s)) {
		panic("bad test json: " + s)
	}
	return base64.StdEncoding.EncodeToString([]byte(s))
}
