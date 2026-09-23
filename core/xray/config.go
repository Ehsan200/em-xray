package xray

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// GenOptions carries the OS-specific bits Generate needs (log paths), keeping
// the rest of config generation pure and deterministic.
type GenOptions struct {
	AccessLog     string
	ErrorLog      string
	LogLevel      string // default "warning"
	ProbeInterval string // burst-observatory ping cadence, e.g. "30s"; default DefaultProbeInterval
	ProbeURL      string // ping destination; default DefaultProbeURL
}

// Generate builds the full xray config.json from entries (outbounds), inbounds
// (listeners routed to a target), and resolved dialer slots (a master's node
// pool). Output is deterministic (all sorted by name, JSON keys sorted by
// encoding/json) so a no-op reconcile is byte-identical and doesn't churn xray.
//
// When any slot exists, the master-dialer machinery is emitted: a gRPC api +
// dokodemo inbound, per-pool slot (socks inbound + dialer outbound per master +
// member outbounds + leastLoad balancer + routing rule), one shared burst
// observatory, and
// the master's own outbound gets streamSettings.sockopt.dialerProxy so its
// server connection tunnels through the fastest node.
func Generate(entries []XrayEntry, inbounds []Inbound, slots []Slot, opts GenOptions) ([]byte, error) {
	ents := append([]XrayEntry(nil), entries...)
	sort.Slice(ents, func(i, j int) bool { return ents[i].Name < ents[j].Name })
	ins := append([]Inbound(nil), inbounds...)
	sort.Slice(ins, func(i, j int) bool { return ins[i].Name < ins[j].Name })
	sl := append([]Slot(nil), slots...)
	sort.Slice(sl, func(i, j int) bool { return sl[i].Index < sl[j].Index })

	// Master name (owner or alias) → slot index, for dialerProxy wiring.
	slotIdx := SlotIndexByName(sl)

	loglevel := opts.LogLevel
	if loglevel == "" {
		loglevel = "warning"
	}

	// block + direct are always present so routing can reference them.
	//
	// ORDER MATTERS AND IS A SECURITY PROPERTY: xray uses the FIRST outbound as
	// its default handler, taken whenever routing yields no tag (an unmatched
	// inbound, a balancer with nothing alive, a dangling tag). If that default
	// were `direct`, every such miss would egress from this box's own IP —
	// silently defeating the tunnel. `block` leads so a routing miss fails
	// CLOSED. `direct` stays reachable, but only by explicit tag, i.e. only for
	// inbounds whose Target really is "direct".
	outbounds := []any{
		map[string]any{"tag": "block", "protocol": "blackhole"},
		map[string]any{"tag": "direct", "protocol": "freedom"},
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
		if e.Mux {
			applyMux(ob)
		}
		// A master tunnels its own server connection through its pool via
		// dialerProxy. A master left without a slot (pool limit reached) is
		// pointed at the blackhole instead: with no dialerProxy it would dial
		// its server straight off this box.
		if _, ok := slotIdx[e.Name]; ok {
			setDialerProxy(ob, DialerTag(e.Name))
		} else if e.IsMaster() {
			setDialerProxy(ob, "block")
		}
		outbounds = append(outbounds, ob)
		haveOut[tag] = true
	}

	inboundsJSON := []any{}
	var rules []any
	for _, in := range ins {
		if !in.Enabled || (in.Port == 0 && !IsUnixInbound(in)) {
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

	// Always emit the gRPC api inbound + stats + traffic policy so per-inbound
	// and per-outbound byte counters are collected (the daemon samples them for
	// the traffic charts), and so every config change can be applied live
	// through the api. The api routing rule must be FIRST so a user catch-all
	// can't swallow api traffic.
	inboundsJSON = append(inboundsJSON, map[string]any{
		"tag": ApiTag, "listen": "127.0.0.1", "port": ApiPort,
		"protocol": "dokodemo-door", "settings": map[string]any{"address": "127.0.0.1"},
	})
	rules = append([]any{map[string]any{
		"type": "field", "inboundTag": []any{ApiTag}, "outboundTag": ApiTag,
	}}, rules...)
	services := []any{"HandlerService", "RoutingService", "StatsService", "ObservatoryService"}
	cfg["stats"] = map[string]any{}
	cfg["metrics"] = map[string]any{"tag": MetricsTag, "listen": "127.0.0.1:" + strconv.Itoa(MetricsPort)}
	cfg["policy"] = map[string]any{
		"system": map[string]any{
			"statsInboundUplink": true, "statsInboundDownlink": true,
			"statsOutboundUplink": true, "statsOutboundDownlink": true,
		},
		// Level 0 = every client (all are emitted at level 0): per-user byte
		// counters for multi-user inbounds, plus an explicit connection
		// policy. xray's default connIdle (300s) silently cuts long-lived but
		// quiet streams — SSH sessions, IMAP IDLE (29 min), push and chat
		// sockets, idle WebSockets — so it is raised past those. Clients that
		// vanish without a FIN are still reaped by TCP keepalive, and a
		// revoked client is cut by the restart that revocation forces, so a
		// long connIdle never lets a disabled or over-quota user linger. The
		// handshake gets 8s for slow mobile links; the half-close timers keep
		// xray's defaults so finished transfers are reaped promptly.
		"levels": map[string]any{
			"0": map[string]any{
				"statsUserUplink": true, "statsUserDownlink": true,
				"handshake": 8, "connIdle": 1800, "uplinkOnly": 2, "downlinkOnly": 5,
			},
		},
	}

	var balancers []any
	{
		for _, s := range sl {
			idx := s.Index
			// slot socks inbound
			inboundsJSON = append(inboundsJSON, map[string]any{
				"tag": SlotInTag(idx), "listen": "127.0.0.1", "port": SlotPort(idx),
				"protocol": "socks", "settings": map[string]any{"udp": true, "auth": "noauth"},
			})
			// one stable dialer outbound per master (owner + aliases), all
			// into the same slot inbound
			for _, master := range s.SlotMasters() {
				outbounds = append(outbounds, map[string]any{
					"tag": DialerTag(master), "protocol": "socks",
					"settings": map[string]any{"servers": []any{
						map[string]any{"address": "127.0.0.1", "port": SlotPort(idx)},
					}},
				})
			}
			// member outbounds (shared slotN-out- prefix → live-adoptable).
			// A member that doesn't tunnel is skipped (the resolver already
			// drops and logs it): it would egress from this box.
			fallback := "block"
			for _, m := range s.Members {
				if CheckMemberOutbound(m.Outbound) != nil {
					continue
				}
				mo, err := MemberOutboundJSON(idx, m)
				if err != nil {
					return nil, fmt.Errorf("master %q: %w", s.Master, err)
				}
				if fallback == "block" {
					fallback = SlotMemberTag(idx, m.Key)
				}
				outbounds = append(outbounds, mo)
			}
			// leastLoad over the prefix reads the burst observatory's rolling
			// ping window and spreads connections over the best
			// SlotBalancerExpected healthy members. No maxRTT cap on purpose:
			// if every node is slow we still want the least-bad ones.
			//
			// fallbackTag covers the moments leastLoad has nothing ranked:
			// right after xray starts (no ping has completed yet) and when
			// every member's recent pings failed. It is the pool's FIRST
			// member: with `block` there, every master connection failed
			// until the first ping landed after each start. A member still
			// egresses through a node, never this box — CheckMemberOutbound
			// guarantees it tunnels — so this is not a leak. Only an empty
			// pool falls back to `block`, keeping the master fail-closed.
			balancers = append(balancers, map[string]any{
				"tag": SlotBalTag(idx), "selector": []any{SlotOutPrefix(idx)},
				"strategy": map[string]any{
					"type":     "leastLoad",
					"settings": map[string]any{"expected": SlotBalancerExpected},
				},
				"fallbackTag": fallback,
			})
			// route slot inbound → balancer
			rules = append(rules, map[string]any{
				"type": "field", "inboundTag": []any{SlotInTag(idx)},
				"balancerTag": SlotBalTag(idx),
			})
		}

		// One shared burst observatory feeds every slot's leastLoad; its
		// prefix selector matches every slot member outbound. Emitted even
		// with no slots: it idles with nothing to match, and having it from
		// the start means the first master a user adds applies live instead
		// of restarting xray (the api can't add an observatory).
		cfg["burstObservatory"] = map[string]any{
			"subjectSelector": []any{ObservatorySelectorPrefix},
			"pingConfig": map[string]any{
				"destination": orDefault(opts.ProbeURL, DefaultProbeURL),
				"interval":    orDefault(opts.ProbeInterval, DefaultProbeInterval),
				"sampling":    DefaultProbeSampling,
				"timeout":     DefaultPingTimeout,
			},
		}
	}
	cfg["api"] = map[string]any{"tag": ApiTag, "services": services}

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
		"protocol": in.Protocol,
	}
	if !IsUnixInbound(in) {
		ib["port"] = in.Port
	}

	switch in.Protocol {
	case "socks":
		set := map[string]any{"udp": true, "auth": "noauth"}
		if in.SocksUser != "" {
			set["auth"] = "password"
			set["accounts"] = []any{map[string]any{"user": in.SocksUser, "pass": in.Password}}
		}
		ib["settings"] = set
		return ib, nil // socks has no transport/security
	case "vless":
		ib["settings"] = map[string]any{"clients": inboundClients(in), "decryption": "none"}
	case "vmess":
		ib["settings"] = map[string]any{"clients": inboundClients(in)}
	case "trojan":
		ib["settings"] = map[string]any{"clients": inboundClients(in)}
	case "hysteria":
		// hysteria is a QUIC transport carrying its own protocol: auth-based
		// users live in settings.clients (xray v26 ignores a "users" key, so no one
		// could authenticate); the transport params (version, primary
		// auth, masquerade) live in streamSettings.hysteriaSettings below.
		ib["settings"] = map[string]any{"version": 2, "clients": hysteriaUsers(in)}
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

// inboundClients builds the clients array: the primary credential plus every
// user in in.Users. Each client carries a stats email (level 0) so xray reports
// per-user byte counters. The daemon has already dropped over-quota users.
func inboundClients(in Inbound) []any {
	email := in.ClientEmail
	if email == "" {
		email = PrimaryUserEmail(in.Name)
	}
	clients := []any{clientObject(in.Protocol, in.UUID, in.Password, email, in.Flow)}
	for _, u := range in.Users {
		clients = append(clients, clientObject(in.Protocol, u.UUID, u.Password, u.Email, in.Flow))
	}
	return clients
}

// hysteriaUsers builds the settings.clients array for a hysteria inbound: the
// primary credential plus every extra user, each an auth string with a level-0
// stats email so xray reports per-user byte counters. The daemon has already
// dropped over-quota users.
func hysteriaUsers(in Inbound) []any {
	users := []any{map[string]any{"auth": in.HysteriaAuth, "email": PrimaryUserEmail(in.Name), "level": 0}}
	for _, u := range in.Users {
		users = append(users, map[string]any{"auth": u.Auth, "email": u.Email, "level": 0})
	}
	return users
}

// clientObject builds one client entry for the given protocol.
func clientObject(protocol, uuid, password, email, flow string) map[string]any {
	c := map[string]any{"level": 0}
	if email != "" {
		c["email"] = email
	}
	switch protocol {
	case "vless":
		c["id"] = uuid
		if flow != "" {
			c["flow"] = flow
		}
	case "vmess":
		c["id"] = uuid
		c["alterId"] = 0
	case "trojan":
		c["password"] = password
	}
	return c
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
		// Hysteria2 is HTTP/3: its clients offer only "h3", and xray's TLS
		// server doesn't add it on its own — without it every hysteria2
		// handshake ends in "tls: no application protocol".
		if network == "hysteria" {
			tls["alpn"] = []any{"h3"}
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
		x := map[string]any{"path": orDefault(in.Path, "/"), "mode": orDefault(in.XHTTPMode, "auto")}
		if in.Host != "" {
			x["host"] = in.Host
		}
		ss["xhttpSettings"] = x
	case "grpc":
		ss["grpcSettings"] = map[string]any{"serviceName": in.Path}
	case "hysteria":
		// version+auth are the transport-level params; auth is the primary
		// credential (overridden per-client by settings.clients). masquerade is
		// omitted → xray serves its default 404 page. udpIdleTimeout left at the
		// xray default (60s).
		hy := map[string]any{"version": 2}
		if in.HysteriaAuth != "" {
			hy["auth"] = in.HysteriaAuth
		}
		ss["hysteriaSettings"] = hy
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
	healAllowInsecure(m)
	return m, nil
}

// healAllowInsecure drops tlsSettings.allowInsecure, which xray v26 removed: a
// config carrying it anywhere is rejected whole, so one entry or pool node
// stored by an older build (whose link parser kept it) would take every
// inbound down. Without it the outbound verifies its certificate normally, or
// by pinnedPeerCertSha256 when the link carried one.
func healAllowInsecure(outbound map[string]any) {
	ss, ok := outbound["streamSettings"].(map[string]any)
	if !ok {
		return
	}
	if tls, ok := ss["tlsSettings"].(map[string]any); ok {
		delete(tls, "allowInsecure")
	}
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
