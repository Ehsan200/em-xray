# em-xray

Single-binary Linux daemon + CLI wrapping [xray-core](https://github.com/XTLS/Xray-core).

Two jobs:

1. **Master dialer** — a subscription URL becomes a live pool of nodes; a "master" outbound tunnels
   through whichever node is currently fastest, with **no xray restart** when the pool changes.
2. **Server** — expose `vless` / `vmess` / `trojan` / `hysteria2` / `socks` listeners (keys, TLS certs,
   share link + QR auto-generated), egressing through a master, with per-user accounts and quotas.

```
your client ──vless/reality──▶ emx inbound ──▶ master ──dialerProxy──▶ fastest node ──▶ internet
                                              (leastLoad over best 2 + burst observatory)
```

The xray binary and geo data are embedded. Interactive menus: run any command group bare (`emx`,
`emx in`, `emx sub`, `emx entry`).

---

## Install

Prebuilt linux/macOS × amd64/arm64 archives are attached to each release.

```bash
case "$(uname -m)" in
  x86_64|amd64) ARCH=amd64 ;;
  aarch64|arm64) ARCH=arm64 ;;
  *) echo "unsupported: $(uname -m)"; exit 1 ;;
esac
LATEST_URL="$(curl -fsSLI -o /dev/null -w '%{url_effective}' https://github.com/Ehsan200/em-xray/releases/latest)"
VERSION="${LATEST_URL##*/}"
DIR="$(mktemp -d)"
curl -fL "https://github.com/Ehsan200/em-xray/releases/download/${VERSION}/emx-${VERSION}-linux-${ARCH}.tar.gz" -o "$DIR/emx.tar.gz"
tar -xzf "$DIR/emx.tar.gz" -C "$DIR"
sudo install -m 0755 "$DIR/emx-linux-${ARCH}/emx" /usr/local/bin/emx
rm -rf "$DIR"
emx version
```

Then run it as a root system service (required if emx manages Caddy):

```bash
sudo emx systemd install --system --now
sudo emx status
sudo emx                      # interactive menu
```

**One machine, one database.** Root uses `/root/.local/share/emx/emx.db` and `/tmp/emx-0`, ignoring
any `HOME` / XDG / `TMPDIR` that sudo passes through, so `sudo emx` and the system service are always
the same instance. Once root state exists, a plain `emx` refuses to open a second, private scope:

```
this machine's emx state belongs to root (/tmp/emx-0); run `sudo emx ...` so both use the same
database, or set EMX_USER_SCOPE=1 to keep a separate per-user daemon
```

Build from source instead (needs Go and the xray assets):

```bash
make fetch-xray TARGET=linux-amd64     # linux-amd64 | linux-arm64 | darwin-amd64 | darwin-arm64
make build                             # ./emx
```

---

## Quick start

```bash
sudo emx start                                  # only if you skipped the systemd unit

sudo emx sub add mysub "https://provider/link"  # 1. node pool
sudo emx entry add mymaster \
    --link "vless://…your-server…" \
    --dialer "xraysub:mysub"                    # 2. master dialing through the pool
sudo emx in add gate --to master:mymaster       # 3. listener for your devices → prints vless://…

sudo emx in qr 1                                # link as a QR code
sudo emx winner                                 # nodes each master currently spreads over
sudo emx traffic                                # per-inbound/outbound charts
sudo emx speed                                  # live ↑/↓ throughput
```

Extra client accounts on one listener, each with its own link and optional quota:

```bash
sudo emx in user add 1 alice --cap 50GB
sudo emx in user ls 1
```

---

## Caddy + Cloudflare + XHTTP

`vless-caddy-xhttp` puts Caddy in front: Caddy owns Cloudflare's proxied ports and terminates TLS,
then forwards VLESS/XHTTP to xray over a Unix socket. The client link uses the domain on port 443.

Generated site addresses: HTTP `80, 8080, 8880, 2052, 2082, 2086, 2095`, HTTPS
`443, 2053, 2083, 2087, 2096, 8443`
([Cloudflare's list](https://developers.cloudflare.com/fundamentals/reference/network-ports/)).

Before starting: point the domain's DNS record at this server, use an SSL/TLS mode that validates the
origin if Cloudflare proxies it, open those TCP ports (80 and 443 at minimum), and run the root
system service.

```bash
sudo emx caddy install                      # official Debian/Ubuntu package
sudo emx in add edge -t vless-caddy-xhttp --domain x.example.com --to direct
```

UUID, email, path, socket and client link are generated; `--to master:NAME` routes through a master
instead of this server's IP. Multiple domains are just multiple inbounds. Override explicitly with
`--path /p/ --email you@example.com --xhttp-mode packet-up`.

emx validates and applies the Caddyfile itself when it runs as root. Manually:

```bash
sudo emx caddy print       # preview the generated Caddyfile
sudo emx caddy domains     # domain, path, inbound
sudo emx caddy apply       # validate, back up, write, reload
sudo emx caddy status | enable | disable
```

`emx caddy apply` **owns** `/etc/caddy/Caddyfile`: it backs the old file up, replaces it, and disables
the Caddy service once no managed inbound remains. Don't point it at a Caddy serving other sites.

Edit an inbound later with `sudo emx in edit <id>` (or *Inbounds → Edit JSON*). Changing UUID, path
or domain means re-importing the client link; changing the email changes the stats key.

---

## Updating

```bash
sudo emx update              # download, replace the binary, restart the daemon
sudo emx update --check      # report only
sudo emx update --proxy tg   # fetch through your own socks inbound named "tg"
```

**Your configuration survives an upgrade.** Older builds could keep the database under an inherited
`HOME` / `XDG_DATA_HOME` and their socket under `/run/user/0/emx`. On first start the daemon **copies**
a database found in one of those layouts into the current location (the original is never deleted),
and `emx update` / `emx restart` stop a daemon still running under the old layout. Verify with:

```bash
sudo emx xray paths          # database / socket / config in use
sudo emx in ls               # should be exactly what you had
```

If it adopted the wrong one, the old file is still there — import it with `sudo emx config import`.

The daemon also checks for releases every 6h and flags it in `emx status`.

Updates are the one thing emx does **not** route through its own tunnel. The download is bounded by
progress, not a stopwatch: it gives up after 60s of silence. If GitHub is unreachable from the box,
`--proxy` takes an inbound name (port + credentials looked up for you; must be enabled, socks, and
aimed at a master or entry), a `HOST:PORT`, or a full `socks5h://` / `http://` URL. Prefer the
environment over flags — a flag is visible in `ps`:

```bash
EMX_PROXY=tg EMX_PROXY_USER=alice EMX_PROXY_PASS=s3cret sudo -E emx update
```

`HTTPS_PROXY` / `HTTP_PROXY` are honoured too.

---

## Concepts

| Thing | What it is |
|---|---|
| **Entry** | An outbound — a remote server this box dials. Needs an inbound to feed it traffic. |
| **Master** | An entry with a `Dialer`: its transport tunnels through a node pool via `dialerProxy`. |
| **Subscription** | A URL yielding a volatile pool of nodes. Only usable inside a master's dialer. |
| **Node** | One pool member, ranked by the burst observatory's rolling pings. Never listens on a port. |
| **Inbound** | A listener you expose, routed to a **Target**. |
| **User** | An extra client on an inbound — own credential/link, own traffic, optional byte cap. |
| **Target** | `master:NAME` (fastest node) · `xray:NAME` (one entry) · `direct` (this server's IP). |

Dialer refs, comma-separated: `xray:NAME`, `xraysub:NAME` (`proxy:NAME` not supported). A master may
mix refs. Masters naming the same refs (in any order) share one slot — one balancer, one set of
member outbounds, one set of probes — so probe cost scales with unique pools, not masters. Each
still keeps its own `dialer-<master>` outbound. Past 32 unique pools a master's dialer is pointed at
the blackhole (fails closed) rather than dialing off this box.

---

## CLI reference

```
emx start | stop | restart | status         daemon lifecycle
emx restart --xray                          cycle only the xray child
emx version

emx sub add <name> <url> | ls | rm <id> | rename <id> <name>
emx sub info <name>                         metadata card: quota, expiry, last fetch
emx sub set <id> [--interval S] [--cap N] [--ua UA]
emx sub enable <id> | disable <id> | refresh [id]
emx sub nodes <id>                          fingerprint, active, disabled, latency
emx sub node-enable | node-disable <subid> <fingerprint>    durable across refreshes
emx sub test <id> [fingerprint]             real latency per node (persisted)

emx entry add <name> --link <share> | --outbound <json> [--dialer <refs>] [--mux]
emx entry mux <id> on|off                   multiplex streams over a few tunnels (per entry)
emx entry ls | rm <id> | rename <id> <name> | duplicate <id> [name] | edit <id>
emx entry test [id]

emx in add [name] [-t TEMPLATE] [--to TARGET] [--host H] [--port N]
                  [--domain D] [--path P] [--email E] [--xhttp-mode MODE]
emx in ls [--links] | rm <id> | qr <id> | duplicate <id> [name] | edit <id>
emx in user add <inbound-id> <name> [--cap 10GB]
emx in user ls <inbound-id> | rm | enable | disable <user-id>
emx in user qr <inbound-id> <user-name>

emx traffic [--window 24h|7d|all] | traffic retention [days]
emx speed                                   live ↑/↓ throughput
emx config export [-o file] | import <file> [--replace]

emx xray config | logs [-a] [-n N] [-f] | logcap [MB] | paths | restart
emx xray reap                               stop orphaned xray left by a killed daemon
emx loglevel [debug|info|warning|error|none]
emx probe-interval [seconds]                observatory ping cadence (default 10)

emx caddy install | print | domains | apply | enable | disable | status
emx template ls | winner | ui
```

Run any group without a subcommand on a terminal for its menu; on a pipe it prints help.

### Templates

REALITY keypairs, self-signed certs (`xray tls cert`, no domain needed), UUIDs and shortIds are all
generated. `emx template ls` for the live list.

Self-signed links **pin the certificate**: `pcs=<sha256>` (xray v26+ `pinnedPeerCertSha256`; hysteria2
`pinSHA256`), plus `allowInsecure=1` / `insecure=1` for clients on an older core. xray v26 refuses a
config that contains `allowInsecure` at all, so emx never puts it in one: links you import are
parsed to `pcs`/`vcn` pinning, and a link with only `allowInsecure` verifies its certificate
normally. Every template's own link is tested end to end against the real xray.

A REALITY inbound borrows its `dest` server's TLS handshake on every connection, so the box must be
able to reach it (default `www.microsoft.com:443`); if it can't, every REALITY connection fails.

| Name | Transport | Security |
|---|---|---|
| `vless-reality` *(default)* | tcp (vision) | REALITY |
| `vless-reality-grpc` / `-xhttp` | gRPC / XHTTP | REALITY |
| `vless-tls` / `-ws` / `-grpc` / `-xhttp` / `-httpupgrade` | tcp / ws / gRPC / XHTTP / HTTPUpgrade | self-signed TLS |
| `vless-caddy-xhttp` | XHTTP over a Unix socket | TLS terminated by Caddy |
| `vmess-tcp` / `vmess-ws` | tcp / websocket | none (CDN-friendly) |
| `vmess-tls-ws` | websocket | self-signed TLS |
| `trojan-tls` / `-ws` | tcp / websocket | self-signed TLS |
| `hysteria2` | QUIC | self-signed TLS |
| `socks` / `socks-public` | tcp loopback / `0.0.0.0` | none / username+password |

`socks-public` prints both a `socks://` link and a `tg://socks?…` link for Telegram. (MTProto proxies
aren't supported — xray-core can't serve them.)

---

## How it works

xray's `dialerProxy` can't point at a balancer, so the tunnel is a loopback cascade:

```
master outbound
  sockopt.dialerProxy → "dialer-<master>"      (stable socks outbound)
    → 127.0.0.1:<slotPort>                     (slot socks inbound)
      → routing: slotN-in → balancerTag slotN-bal
        → leastLoad spreads over the best 2 slotN-out-<key> members → the node outbound
```

Members share the `slotN-out-` tag **prefix**, so the balancer and the shared observatory adopt
live-added members with no reload. A pool keeps its slot index across changes, so adding a master
never renumbers another pool's slot.

**xray is restarted only when unavoidable.** Every change — adding/editing/removing an entry,
inbound, user or master, a subscription refresh, a node toggle — regenerates the config, diffs it
against the one xray is running, and applies the difference through the api: inbounds/outbounds
replaced by tag (`adi`/`rmi`/`ado`/`rmo`), routing rules + balancers swapped whole (`adrules`).
Connections through anything that didn't change are untouched. A restart happens only when:

- a section the api can't change differs (log level, probe interval, policy, stats, metrics);
- a client credential is **revoked** (user disabled/removed/over quota, password or UUID changed,
  authenticated inbound removed) — xray keeps sessions an inbound already accepted across an api
  replace, so only a restart actually cuts the revoked client off;
- or an api call fails (the restart converges from any partial state).

`emx status` shows how many changes went live vs. needed a restart.

### Fail closed, never direct

An inbound routed through a master must never egress from this box's IP. The rules:

- `block` is `outbounds[0]`, so any routing miss hits a blackhole (xray's default handler).
- A pool member must be a proxy protocol (vless, vmess, trojan, shadowsocks, socks, http, hysteria,
  wireguard). A `freedom` entry is refused as a dialer ref and dropped from a pool if edited later.
- Each balancer falls back to the pool's **first member** while `leastLoad` has nothing ranked (right
  after a start, or when every recent ping failed) — so a restart doesn't black out masters until
  the first ping — and to `block` only when the pool is empty.
- An enabled master always gets a slot, even with zero members, so the `dialerProxy` hop never
  disappears.
- A live apply adds before it removes, so a full pool rotation is never empty mid-flight, and takes
  a master down before replacing its `dialer-<master>` outbound (bringing it back after), so a master
  never exists without its dialer hop.

`emx status` and `emx winner` report how many pool members answer pings and which ones the master
is spread over; `emx in ls` marks the inbounds
that legitimately use this server's IP. The burst observatory pings every member every 10s (window
of 3, 5s timeout) and `leastLoad` drops a failing node on its next ping; `emx probe-interval`
changes the cadence (5–3600s).

### Health, testing, accounting

- **xray checks every config before it is applied** (`xray run -test`). A pool node it refuses
  (a field this xray version rejects, a broken transport) is left out of its pool with xray's reason
  — `emx sub nodes <id>` shows it — and everything else applies. Any other refused change (an entry,
  an inbound) is not applied at all: the running xray keeps serving, and `emx status` plus the
  command's error name the object and the reason.

- **Watchdog** restarts a crashed xray with backoff. Separately the daemon calls xray's local stats
  API every 30s and force-restarts after three consecutive failures; `emx status` shows
  `health: responsive` and the health-restart count.
- **Dead nodes are parked.** xray's metrics endpoint (loopback, from `11933`) exposes the burst
  observatory's per-node pings; every 30s the daemon reads it, and a node whose every ping failed
  for 15 min is left out of its pool (a live outbound removal). It returns on trial after 30 min,
  doubling per failed trial up to 6h; one good ping clears its record. Never parked without a live
  sibling in the same pool (all-dead = this box's uplink) and never the last member. In memory
  only. `emx sub nodes <id>` shows parked nodes, `emx status` the count.
- **No orphans**: the xray child gets `Pdeathsig`, so a SIGKILLed or OOM-killed daemon takes it down
  too. Any orphan that predates this is reaped when the daemon starts and by `emx update` /
  `emx restart`; `emx xray reap` does it on demand. Orphans matter because the listeners share the
  port, so an old one keeps answering a share of the connections with an old config. Only xray
  started from emx's own cache path is ever signalled.
- **`emx sub test` / `emx entry test`** never touch the live xray: a throwaway xray gets one no-auth
  socks inbound per config on ephemeral loopback ports (a master's `dialerProxy` hop is stripped) and
  the probe URL is fetched through each concurrently, in batches of 24 with halving retry. Results
  persist per fingerprint. Different measurement from `emx winner`, which is the observatory's.
- **Mux (opt-in per entry)**: `emx entry mux <id> on` turns connections through that entry into
  streams inside a few long-lived tunnels (no fresh handshake per connection; shared fate if a
  tunnel breaks). Applied only to VMess, VLESS without an XTLS flow, and Trojan over
  non-multiplexing transports (not gRPC/XHTTP/H2/QUIC), never over a `mux` block you wrote, with
  UDP/443 skipped; `emx entry ls` says why it doesn't apply to an entry.
- **Connection policy**: idle connections live 30 min (`connIdle 1800`, xray's default 300s cut SSH,
  IMAP IDLE and push sockets), 8s handshake, xray's default half-close timers. Byte accounting is
  unaffected, and quota cut-offs restart xray, so a revoked user never lingers on an idle socket.
- **Traffic**: stats are always enabled; counters are sampled every minute (reset-safe) into hourly
  buckets + lifetime totals. The gRPC api binds a loopback port from `11932`, advancing if taken.
- **Bounded disk**: logs roll at `emx xray logcap` (default 50 MB each, `0` disables); hourly buckets
  are pruned to `emx traffic retention` (default 8 days).

### Paths

```
data     $XDG_DATA_HOME/emx      sqlite database
state    $XDG_STATE_HOME/emx     xray access/error logs
cache    $XDG_CACHE_HOME/emx     extracted xray binary + geo data
runtime  $XDG_RUNTIME_DIR/emx    control socket, pid, generated config.json
```

Root pins data/config/state/cache under `/root` and runtime under `/tmp/emx-0`, ignoring XDG and
`TMPDIR`. Other users follow XDG, falling back to `/tmp/emx-<uid>`. A daemon starting on a new layout
adopts a database left by an older one, and a non-root run defers to the root instance
(`EMX_USER_SCOPE=1` overrides).

The daemon needs no privileges of its own — loopback listeners plus a child process — but root is
required for Caddy and `/etc/caddy/Caddyfile`.

---

## Development

```bash
make test          # unit tests (core/ is OS-agnostic)
make vet
make proto         # regenerate gRPC stubs after editing api/emx.proto
make build
make release BUMP=patch
```

Config generation is verified against the real xray binary (`xray -test`, binds nothing). The live
routing test is opt-in — it binds fixed loopback ports:

```bash
EMX_E2E=1 go test ./daemon/ -run EndToEnd
```

```
cmd/emx/     cobra CLI, bubbletea TUI, gRPC client
core/xray/   OS-agnostic: models, store, link parser, config Generate, dialer, keygen, templates
daemon/      supervisor, watchdog, xray api, gRPC server, scheduler, traffic sampler
api/         emx.proto + generated stubs
internal/    paths (XDG), xraybin (go:embed)
```

---

## License

[MIT](LICENSE)
