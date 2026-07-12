# em-xray

A single-binary Linux daemon + CLI that turns [xray-core](https://github.com/XTLS/Xray-core)
into a **master dialer with subscription-backed node pools**: subscription URLs become a live pool
of nodes, and a "master" outbound tunnels its own server connection through **whichever node in the
pool is currently fastest** — with **zero xray restarts** on node churn.

It also runs as a **server**: expose a `vless` / `vmess` / `trojan` / `hysteria2` / `socks` listener (keys +
self-signed TLS certs auto-generated, client share-link + QR printed) whose traffic egresses through a
master's fastest node — with **per-user accounts, byte quotas, and live traffic charts**.

```
your client ──vless/reality──▶ emx inbound ──▶ master ──dialerProxy──▶ fastest node ──▶ internet
                                                         (leastPing balancer + observatory)
```

---

## Highlights

- **Fastest-node routing** — subscription nodes are probed by xray's observatory; a `leastPing`
  balancer picks the winner. Nodes never listen on ports; they're outbounds ranked by live latency.
- **Zero-restart churn** — a subscription refresh, node enable/disable, or cap change updates the
  live pool over xray's gRPC API (`ado`/`rmo`), never restarting xray or dropping connections.
- **Server mode** — create an inbound from 15 built-in templates (vless/vmess/trojan × reality/TLS ×
  tcp/ws/grpc/xhttp/httpupgrade, plus hysteria2/QUIC) with one command; UUID, REALITY keypair, and **self-signed TLS
  certs** (via `xray tls cert`, no domain needed) are auto-generated and the client share-link + a
  scannable **QR code** are printed.
- **Multi-user + quotas** — add many client accounts to one listener, each with its own UUID/link and
  an optional **byte cap**; a user that hits their quota is dropped from the config automatically.
- **Traffic charts** — per-inbound/outbound byte totals (24h / all-time) as terminal sparklines, a
  **live ↑/↓ speed meter**, and per-user usage. xray's stats API is sampled into hourly buckets.
- **Backup / restore** — export the whole config (inbounds + entries + subscriptions) to one JSON
  file and import it back, merge or replace.
- **Self-managed daemon** — `emx start` detaches into the background, supervises the xray child, and
  restarts it on crash with exponential backoff. No systemd, no root required.
- **Interactive TUI** — run any command bare (`emx`, `emx sub`, `emx in`…) for arrow-key menus.
  Everything is selectable; you never type an ID or a node fingerprint.
- **Single binary** — the matching xray-core binary + geo data are embedded via `go:embed`.

---

## Install

### Download a release

Prebuilt binaries for linux/macOS × amd64/arm64 are attached to each GitHub release.
The xray binary + geo data are bundled inside the archive — nothing else to fetch.

```bash
VERSION=v1.0.0                       # pick a tag from the releases page
OS=linux                             # linux | darwin
ARCH=amd64                           # amd64 | arm64

curl -LO "https://github.com/Ehsan200/em-xray/releases/download/$VERSION/emx-$VERSION-$OS-$ARCH.tar.gz"
tar -xzf "emx-$VERSION-$OS-$ARCH.tar.gz"          # extracts emx-$OS-$ARCH/
sudo install "emx-$OS-$ARCH/emx" /usr/local/bin/emx
emx version
```

One-liner for the latest linux/amd64 build:

```bash
curl -sL https://api.github.com/repos/Ehsan200/em-xray/releases/latest \
  | grep -o 'https://[^"]*linux-amd64\.tar\.gz' \
  | xargs curl -L | tar -xz
```

### Build from source

Requires Go 1.26+. The xray binary is fetched at build time (it is not committed).

```bash
git clone <this-repo> em-xray && cd em-xray
make fetch-xray            # downloads xray v26.3.27 for linux-64 (override with TARGET=)
make build                 # produces ./emx
```

Cross-target builds fetch the matching xray triple, e.g. for local macOS testing:

```bash
make fetch-xray TARGET=macos-arm64-v8a
```

---

## Quick start

```bash
emx start                                  # launch the background daemon

# 1. add a subscription (node pool)
emx sub add mysub "https://provider/link"

# 2. add a master: your own server (from a share link) that dials through the pool's fastest node
emx entry add mymaster \
    --link "vless://…your-server…" \
    --dialer "xraysub:mysub"

# 3. expose a server for your devices, egressing through the master
emx in add gate --to master:mymaster       # prints a client vless://… link
emx in qr 1                                # same link as a scannable QR code

# see which node is currently fastest, and how much traffic has flowed
emx winner
emx traffic                                # per-inbound/outbound charts (24h + all-time)
emx speed                                  # live ↑/↓ throughput, Ctrl-C to stop
```

Add extra client accounts to a listener, each with its own link and an optional quota:

```bash
emx in user add 1 alice --cap 50GB         # own uuid/link; access denied once 50 GB is used
emx in user ls 1                           # usage vs cap per user
```

Prefer menus? Just run `emx` (or `emx sub`, `emx in`, `emx entry`) with no arguments.

### Keep it running (systemd)

`emx start` supervises the xray child itself, but to survive a **reboot or a daemon crash**, install a
systemd service:

```bash
emx systemd install --now        # user service (default); starts + enables it
loginctl enable-linger "$USER"   # keep it running across reboots without a login session

# system-wide (needs root) instead:
sudo emx systemd install --system --now
```

`emx systemd print` shows the unit without installing; `emx systemd uninstall` removes it.

### Updating

```bash
emx update           # check GitHub, download the matching build, swap the binary, restart the daemon
emx update --check   # just report whether a newer release exists
```

The running daemon also checks for new releases every 6h and flags it in `emx status`.

---

## Concepts

| Thing | What it is |
|---|---|
| **Entry** | An outbound — a remote server this box dials. Pure config; needs an inbound to feed it traffic. |
| **Master** | An entry with a `Dialer`. Its transport tunnels through a node pool via `dialerProxy`. |
| **Subscription** | A remote URL yielding a volatile pool of nodes. Never a route target by itself — consumed only inside a master's dialer. |
| **Node** | One member of a subscription pool. Ranked fastest-first by the observatory. Never listens on a port. |
| **Inbound** | A server listener (`vless`/`vmess`/`socks`/`trojan`/`hysteria`) you expose, routed to a **Target**. |
| **User** | An extra client on an inbound — its own credential/link, per-user traffic, and an optional byte cap. The inbound's own key is the primary client. |
| **Target** | Where an inbound egresses: `master:NAME` (fastest node) · `xray:NAME` (one entry) · `direct`. |

**Dialer refs** (comma-separated): `xray:NAME` (one entry), `xraysub:NAME` (a subscription's active
nodes), `proxy:NAME` (not yet supported). A master may mix refs, and you may run many masters — each
gets its own slot + balancer.

---

## CLI reference

```
emx start | stop | restart | status        daemon lifecycle
emx version                                 emx + embedded xray versions

emx sub add <name> <url>                    add + fetch a subscription
emx sub ls | rm <id> | rename <id> <name>
emx sub info <name>                         metadata card: quota, expiry, last fetch
emx sub set <id> [--interval S] [--cap N] [--ua UA]   change refresh options
emx sub enable <id> | disable <id>
emx sub refresh [id]                        refresh one (or all); reports +added/-removed
emx sub nodes <id>                          list nodes (fingerprint, active, disabled, latency)
emx sub node-disable <subid> <fingerprint>  durable — survives refresh/restart
emx sub node-enable  <subid> <fingerprint>

emx entry add <name> --link <share> | --outbound <json> [--dialer <refs>]
emx entry ls | rm <id> | rename <id> <name>
emx entry duplicate <id> [name] | edit <id>            clone / edit outbound JSON in $EDITOR

emx in add [name] [--template T] [--to TARGET] [--host H] [--port N]
emx in ls [--links] | rm <id>
emx in qr <id>                              share link as a scannable QR code
emx in duplicate <id> [name]                clone (fresh keys + port)
emx in edit <id>                            edit the inbound JSON in $EDITOR
emx in user add <inbound-id> <name> [--cap 10GB]      add a client with own link/quota
emx in user ls <inbound-id>                 users + usage vs cap
emx in user rm <user-id> | enable <user-id> | disable <user-id>
emx in user qr <inbound-id> <user-name>     a user's link as a QR code

emx traffic [--window 24h|7d|all]           per-inbound/outbound charts (totals + sparkline)
emx traffic retention [days]                how long to keep hourly history (default 8)
emx speed                                   live ↑/↓ throughput; Ctrl-C to stop
emx config export [-o file] | import <file> [--replace]   backup / restore all config

emx xray config                             print the generated xray config.json
emx xray logs [-a] [-n N] [-f]              tail xray's error (or --access) log
emx xray logcap [MB]                        per-file log size cap (0 disables; default 50)
emx xray paths                              show the XDG paths in use
emx loglevel [debug|info|warning|error|none]   show or change the xray log level

emx template ls                             built-in inbound presets
emx winner                                  current fastest node per master
emx ui                                      open the interactive menu
```

Any command group run without a subcommand on a terminal opens its interactive menu. On a
non-terminal (pipes, scripts) it prints help, so flag-driven usage stays scriptable.

### Templates

Everything is auto-generated — REALITY keypairs, self-signed TLS certs (via `xray tls cert`, no domain
required; the client link carries `allowInsecure`), UUIDs, shortIds. Run `emx template ls` for the
live list.

| Name | Transport | Security |
|---|---|---|
| `vless-reality` *(default)* | tcp (vision) | REALITY — best against active probing |
| `vless-reality-grpc` | gRPC | REALITY |
| `vless-reality-xhttp` | XHTTP | REALITY |
| `vless-tls` | tcp (vision) | self-signed TLS |
| `vless-tls-ws` | websocket | self-signed TLS |
| `vless-tls-xhttp` | XHTTP | self-signed TLS |
| `vless-tls-grpc` | gRPC | self-signed TLS |
| `vless-tls-httpupgrade` | HTTPUpgrade | self-signed TLS |
| `vmess-ws` | websocket | none (CDN-friendly) |
| `vmess-tcp` | tcp | none |
| `vmess-tls-ws` | websocket | self-signed TLS |
| `trojan-tls` | tcp | self-signed TLS |
| `trojan-tls-ws` | websocket | self-signed TLS |
| `hysteria2` | hysteria (QUIC) | self-signed TLS — UDP, fast on lossy links |
| `socks` | tcp (loopback) | none — local proxy |

---

## How it works

xray's `dialerProxy` can't point at a balancer directly, so the tunnel is a loopback cascade:

```
master outbound
  streamSettings.sockopt.dialerProxy → "dialer-<master>"        (stable socks outbound)
      → 127.0.0.1:<slotPort>                                    (slot socks inbound)
          → routing: inboundTag slotN-in → balancerTag slotN-bal
              → leastPing balancer selects among  slotN-out-<key>  members
                  → the actual node outbound
```

One shared observatory (`subjectSelector: ["slot"]`) probes every member. Because members share the
`slotN-out-` tag **prefix**, the balancer and observatory adopt live-added members with no config
reload — that's the zero-restart trick. A subscription refresh diffs the pool and applies the delta
with `xray api ado/rmo`; only a change to the *set* of masters triggers a full config regen + restart.

### Traffic accounting

xray's stats API is always enabled (`policy.system.stats*` + per-user `levels.0.statsUser*`). The
daemon samples the cumulative byte counters once a minute, diffs them (reset-safe across xray
restarts), and stores hourly buckets + lifetime totals in sqlite — that feeds `emx traffic`, the
per-user usage, and byte-cap enforcement. The gRPC api binds a loopback port starting at `11932`,
**auto-advancing** if it's taken (so it never clashes with another xray on the same host).

### Disk usage is bounded

- **Logs** — xray's access/error logs are capped (`emx xray logcap`, default **50 MB** each). Past the
  cap the file is rolled to `*.prev` and truncated in place — xray keeps writing, no restart. Peak per
  log is ~2× the cap. Set `0` to disable rotation.
- **Traffic metadata** — hourly buckets are pruned to a rolling window (`emx traffic retention`,
  default **8 days**); lifetime totals are a single row per inbound/outbound/user. Subscription nodes
  are volatile (replaced each fetch, capped by the sub's node cap). Nothing grows unbounded.

### Paths (XDG)

```
data     $XDG_DATA_HOME/emx      sqlite database
state    $XDG_STATE_HOME/emx     xray access/error logs
cache    $XDG_CACHE_HOME/emx     extracted xray binary + geo data
runtime  $XDG_RUNTIME_DIR/emx    control socket, pid, generated config.json
```

The daemon needs **no elevated privileges** — it's all loopback SOCKS + a child process. The CLI
talks to the daemon over a gRPC unix socket.

---

## Development

```bash
make test          # unit tests (core/ is OS-agnostic; runs anywhere)
make vet
make proto         # regenerate gRPC stubs after editing api/emx.proto
make build

make release BUMP=patch   # bump + push a release tag (builds all platforms via CI)
```

Config generation is verified against the real xray binary with `xray -test` (validate-only, binds
nothing). The live end-to-end routing test is opt-in — it binds fixed loopback ports:

```bash
EMX_E2E=1 go test ./daemon/ -run EndToEnd
```

### Layout

```
cmd/emx/          cobra CLI, bubbletea TUI, gRPC client (traffic/QR/backup/users views)
core/xray/        OS-agnostic: models, store, link parser, config Generate, dialer, keygen, templates,
                  traffic stats, multi-user
daemon/           supervisor (Reconcile / SyncDialerMembers), watchdog, xray api, gRPC server,
                  scheduler, traffic sampler, backup/user handlers
api/              emx.proto + generated stubs
internal/         paths (XDG), xraybin (go:embed)
scripts/          fetch-xray.sh
```

---

## License

[MIT](LICENSE)
