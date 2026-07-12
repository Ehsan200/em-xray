package xray

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// GenOptions carries the OS-specific bits Generate needs (log paths), keeping
// the rest of config generation pure and deterministic.
type GenOptions struct {
	AccessLog string
	ErrorLog  string
	LogLevel  string // default "warning"
}

// Generate builds the full xray config.json from entries (outbounds), inbounds
// (listeners routed to a target), and resolved dialer slots (a master's node
// pool). Output is deterministic (all sorted by name, JSON keys sorted by
// encoding/json) so a no-op reconcile is byte-identical and doesn't churn xray.
//
// When any slot exists, the master-dialer machinery is emitted: a gRPC api +
// dokodemo inbound, per-master slot (socks inbound + dialer outbound + member
// outbounds + leastPing balancer + routing rule), one shared observatory, and
// the master's own outbound gets streamSettings.sockopt.dialerProxy so its
// server connection tunnels through the fastest node.
func Generate(entries []XrayEntry, inbounds []Inbound, slots []Slot, opts GenOptions) ([]byte, error) {
	ents := append([]XrayEntry(nil), entries...)
	sort.Slice(ents, func(i, j int) bool { return ents[i].Name < ents[j].Name })
	ins := append([]Inbound(nil), inbounds...)
	sort.Slice(ins, func(i, j int) bool { return ins[i].Name < ins[j].Name })
	sl := append([]Slot(nil), slots...)
	sort.Slice(sl, func(i, j int) bool { return sl[i].Master < sl[j].Master })

	// Master name → slot index (sorted-name order), for dialerProxy wiring.
	slotIdx := make(map[string]int, len(sl))
	for i, s := range sl {
		slotIdx[s.Master] = i
	}

	loglevel := opts.LogLevel
	if loglevel == "" {
		loglevel = "warning"
	}

	// direct + block are always present so routing can reference them.
	outbounds := []any{
		map[string]any{"tag": "direct", "protocol": "freedom"},
		map[string]any{"tag": "block", "protocol": "blackhole"},
	}
	haveOut := map[string]bool{"direct": true, "block": true}
	for _, e := range ents {
		if !e.Enabled {
			continue
		}
		tag := "out-" + sanitizeKey(e.Name)
		ob, err := entryOutbound(e.Outbound, tag)
		if err != nil {
			return nil, fmt.Errorf("entry %q: %w", e.Name, err)
		}
		// A master with a populated slot tunnels its own server connection
		// through the pool via dialerProxy.
		if _, ok := slotIdx[e.Name]; ok {
			setDialerProxy(ob, DialerTag(e.Name))
		}
		outbounds = append(outbounds, ob)
		haveOut[tag] = true
	}

	inboundsJSON := []any{}
	var rules []any
	for _, in := range ins {
		if !in.Enabled || in.Port == 0 {
			continue
		}
		ib, err := buildInbound(in)
		if err != nil {
			return nil, fmt.Errorf("inbound %q: %w", in.Name, err)
		}
		inboundsJSON = append(inboundsJSON, ib)

		outTag, err := outboundTagFor(in.Target)
		if err != nil {
			return nil, fmt.Errorf("inbound %q: %w", in.Name, err)
		}
		if !haveOut[outTag] {
			return nil, fmt.Errorf("inbound %q targets %q but no such outbound", in.Name, in.Target)
		}
		rules = append(rules, map[string]any{
			"type":        "field",
			"inboundTag":  []any{"in-" + sanitizeKey(in.Name)},
			"outboundTag": outTag,
		})
	}

	cfg := map[string]any{
		"log": map[string]any{
			"access":   opts.AccessLog,
			"error":    opts.ErrorLog,
			"loglevel": loglevel,
		},
	}

	var balancers []any
	if len(sl) > 0 {
		// gRPC api on a dokodemo inbound; its routing rule must be FIRST so a
		// user catch-all can't swallow api traffic.
		inboundsJSON = append(inboundsJSON, map[string]any{
			"tag": ApiTag, "listen": "127.0.0.1", "port": ApiPort,
			"protocol": "dokodemo-door", "settings": map[string]any{"address": "127.0.0.1"},
		})
		rules = append([]any{map[string]any{
			"type": "field", "inboundTag": []any{ApiTag}, "outboundTag": ApiTag,
		}}, rules...)

		for idx, s := range sl {
			// slot socks inbound
			inboundsJSON = append(inboundsJSON, map[string]any{
				"tag": SlotInTag(idx), "listen": "127.0.0.1", "port": SlotPort(idx),
				"protocol": "socks", "settings": map[string]any{"udp": true, "auth": "noauth"},
			})
			// stable dialer outbound → slot inbound
			outbounds = append(outbounds, map[string]any{
				"tag": DialerTag(s.Master), "protocol": "socks",
				"settings": map[string]any{"servers": []any{
					map[string]any{"address": "127.0.0.1", "port": SlotPort(idx)},
				}},
			})
			// member outbounds (shared slotN-out- prefix → live-adoptable)
			for _, m := range s.Members {
				mo, err := MemberOutboundJSON(idx, m)
				if err != nil {
					return nil, fmt.Errorf("master %q: %w", s.Master, err)
				}
				outbounds = append(outbounds, mo)
			}
			// leastPing balancer over the prefix
			balancers = append(balancers, map[string]any{
				"tag": SlotBalTag(idx), "selector": []any{SlotOutPrefix(idx)},
				"strategy": map[string]any{"type": "leastPing"},
			})
			// route slot inbound → balancer
			rules = append(rules, map[string]any{
				"type": "field", "inboundTag": []any{SlotInTag(idx)},
				"balancerTag": SlotBalTag(idx),
			})
		}

		cfg["api"] = map[string]any{
			"tag":      ApiTag,
			"services": []any{"HandlerService", "RoutingService", "ObservatoryService", "StatsService"},
		}
		cfg["stats"] = map[string]any{}
		cfg["observatory"] = map[string]any{
			"subjectSelector":   []any{ObservatorySelectorPrefix},
			"probeURL":          DefaultProbeURL,
			"probeInterval":     DefaultProbeInterval,
			"enableConcurrency": true,
		}
	}

	routing := map[string]any{"rules": rules}
	if len(balancers) > 0 {
		routing["balancers"] = balancers
	}
	cfg["inbounds"] = inboundsJSON
	cfg["outbounds"] = outbounds
	cfg["routing"] = routing
	return json.MarshalIndent(cfg, "", "  ")
}

// setDialerProxy sets streamSettings.sockopt.dialerProxy on an outbound map,
// creating the nested objects as needed.
func setDialerProxy(outbound map[string]any, dialerTag string) {
	ss, ok := outbound["streamSettings"].(map[string]any)
	if !ok {
		ss = map[string]any{}
		outbound["streamSettings"] = ss
	}
	sockopt, ok := ss["sockopt"].(map[string]any)
	if !ok {
		sockopt = map[string]any{}
		ss["sockopt"] = sockopt
	}
	sockopt["dialerProxy"] = dialerTag
}

// outboundTagFor maps an inbound Target to the outbound tag its traffic exits by.
func outboundTagFor(target string) (string, error) {
	kind, name, err := ParseTarget(target)
	if err != nil {
		return "", err
	}
	switch kind {
	case TargetDirect:
		return "direct", nil
	case KindXray, KindMaster:
		// Both route to the entry's outbound tag; a master's outbound additionally
		// carries the dialerProxy sockopt (added in P5).
		return "out-" + sanitizeKey(name), nil
	default:
		return "", fmt.Errorf("unhandled target kind %q", kind)
	}
}

// buildInbound assembles the xray inbound object for a listener.
func buildInbound(in Inbound) (map[string]any, error) {
	tag := "in-" + sanitizeKey(in.Name)
	listen := in.Listen
	if listen == "" {
		listen = "0.0.0.0"
	}
	ib := map[string]any{
		"tag":      tag,
		"listen":   listen,
		"port":     in.Port,
		"protocol": in.Protocol,
	}

	switch in.Protocol {
	case "socks":
		ib["settings"] = map[string]any{"udp": true, "auth": "noauth"}
		return ib, nil // socks has no transport/security
	case "vless":
		client := map[string]any{"id": in.UUID}
		if in.Flow != "" {
			client["flow"] = in.Flow
		}
		ib["settings"] = map[string]any{"clients": []any{client}, "decryption": "none"}
	case "vmess":
		ib["settings"] = map[string]any{"clients": []any{map[string]any{"id": in.UUID, "alterId": 0}}}
	case "trojan":
		ib["settings"] = map[string]any{"clients": []any{map[string]any{"password": in.Password}}}
	default:
		return nil, fmt.Errorf("unsupported inbound protocol %q", in.Protocol)
	}

	ss, err := buildInboundStream(in)
	if err != nil {
		return nil, err
	}
	if len(ss) > 0 {
		ib["streamSettings"] = ss
	}
	// Server inbounds sniff so routing/domain rules can see the real destination.
	ib["sniffing"] = map[string]any{"enabled": true, "destOverride": []any{"http", "tls"}}
	return ib, nil
}

// buildInboundStream builds the server-side streamSettings for an inbound.
func buildInboundStream(in Inbound) (map[string]any, error) {
	network := in.Network
	if network == "" {
		network = "tcp"
	}
	ss := map[string]any{"network": network}

	switch in.Security {
	case "reality":
		if in.RealityPrivateKey == "" {
			return nil, fmt.Errorf("reality selected but no private key")
		}
		ss["security"] = "reality"
		ss["realitySettings"] = map[string]any{
			"show":        false,
			"dest":        orDefault(in.RealityDest, "www.microsoft.com:443"),
			"xver":        0,
			"serverNames": []any{orDefault(in.RealitySNI, "www.microsoft.com")},
			"privateKey":  in.RealityPrivateKey,
			"shortIds":    []any{in.RealityShortID},
		}
	case "tls":
		if in.TLSCert == "" || in.TLSKey == "" {
			return nil, fmt.Errorf("tls selected but no certificate")
		}
		ss["security"] = "tls"
		tls := map[string]any{
			"certificates": []any{map[string]any{
				"certificate": pemLines(in.TLSCert),
				"key":         pemLines(in.TLSKey),
			}},
		}
		if in.TLSSNI != "" {
			tls["serverName"] = in.TLSSNI
		}
		ss["tlsSettings"] = tls
	case "", "none":
		// plain
	}

	switch network {
	case "ws":
		ws := map[string]any{"path": orDefault(in.Path, "/")}
		if in.Host != "" {
			ws["headers"] = map[string]any{"Host": in.Host}
		}
		ss["wsSettings"] = ws
	case "httpupgrade":
		hu := map[string]any{"path": orDefault(in.Path, "/")}
		if in.Host != "" {
			hu["host"] = in.Host
		}
		ss["httpupgradeSettings"] = hu
	case "xhttp":
		x := map[string]any{"path": orDefault(in.Path, "/"), "mode": "auto"}
		if in.Host != "" {
			x["host"] = in.Host
		}
		ss["xhttpSettings"] = x
	case "grpc":
		ss["grpcSettings"] = map[string]any{"serviceName": in.Path}
	}
	return ss, nil
}

// pemLines splits a PEM blob into its lines (no trailing empties) for xray's
// tlsSettings.certificates, which take certificate/key as string arrays.
func pemLines(pem string) []any {
	out := []any{}
	for _, ln := range strings.Split(strings.TrimRight(pem, "\n"), "\n") {
		out = append(out, ln)
	}
	return out
}

// entryOutbound unmarshals a stored outbound, forces its tag, and heals a
// legacy xhttp `extra` field. Returns the outbound as a generic map.
func entryOutbound(raw, tag string) (map[string]any, error) {
	var m map[string]any
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		return nil, fmt.Errorf("bad outbound json: %w", err)
	}
	m["tag"] = tag
	healXHTTPExtra(m)
	return m, nil
}

// healXHTTPExtra fixes a legacy import quirk: xhttp
// streamSettings.xhttpSettings.extra was stored as a JSON *string*, but xray
// wants an *object* (conf.SplitHTTPConfig). Parse the string into an object, or
// drop it if unparseable — otherwise xray refuses to start. Applied to entry
// outbounds AND (in P5) slot member outbounds so a live-added member is
// byte-identical to the baked one.
func healXHTTPExtra(outbound map[string]any) {
	ss, ok := outbound["streamSettings"].(map[string]any)
	if !ok {
		return
	}
	xs, ok := ss["xhttpSettings"].(map[string]any)
	if !ok {
		return
	}
	extra, ok := xs["extra"]
	if !ok {
		return
	}
	s, ok := extra.(string)
	if !ok {
		return
	}
	var obj map[string]any
	if err := json.Unmarshal([]byte(s), &obj); err == nil {
		xs["extra"] = obj
	} else {
		delete(xs, "extra")
	}
}
