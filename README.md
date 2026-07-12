# em-xray

A single-binary Linux daemon + CLI that turns [xray-core](https://github.com/XTLS/Xray-core)
into a **master dialer with subscription-backed node pools**: subscription URLs become a live pool
of nodes, and a "master" outbound tunnels its own server connection through **whichever node in the
pool is currently fastest** — with **zero xray restarts** on node churn.

It also runs as a **server**: expose a `vless` / `vmess` / `socks` listener (keys auto-generated,
client share-link printed) whose traffic egresses through a master's fastest node.

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
- **Server mode** — create a `vless-reality` / `vmess` / `socks` inbound with one command; UUID,
  REALITY keypair and shortId are auto-generated and the client share-link is printed.
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

curl -LO "https://github.com/gravisun/em-xray/releases/download/$VERSION/emx-$VERSION-$OS-$ARCH.tar.gz"
tar -xzf "emx-$VERSION-$OS-$ARCH.tar.gz"          # extracts emx-$OS-$ARCH/
sudo install "emx-$OS-$ARCH/emx" /usr/local/bin/emx
emx version
```

One-liner for the latest linux/amd64 build:

```bash
curl -sL https://api.github.com/repos/gravisun/em-xray/releases/latest \
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

# see which node is currently fastest
emx winner
```

Prefer menus? Just run `emx` (or `emx sub`, `emx in`, `emx entry`) with no arguments.

---

## Concepts

| Thing | What it is |
|---|---|
| **Entry** | An outbound — a remote server this box dials. Pure config; needs an inbound to feed it traffic. |
| **Master** | An entry with a `Dialer`. Its transport tunnels through a node pool via `dialerProxy`. |
| **Subscription** | A remote URL yielding a volatile pool of nodes. Never a route target by itself — consumed only inside a master's dialer. |
| **Node** | One member of a subscription pool. Ranked fastest-first by the observatory. Never listens on a port. |
| **Inbound** | A server listener (`vless`/`vmess`/`socks`/`trojan`) you expose, routed to a **Target**. |
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
emx sub enable <id> | disable <id>
emx sub refresh [id]                        refresh one (or all if omitted)
emx sub nodes <id>                          list nodes (fingerprint, active, disabled, latency)
emx sub node-disable <subid> <fingerprint>  durable — survives refresh/restart
emx sub node-enable  <subid> <fingerprint>

emx entry add <name> --link <share> | --outbound <json> [--dialer <refs>]
emx entry ls | rm <id> | rename <id> <name>

emx in add [name] [--template T] [--to TARGET] [--host H] [--port N]
emx in ls [--links] | rm <id>

emx template ls                             built-in inbound presets
emx winner                                  current fastest node per master
emx ui                                      open the interactive menu
```

Any command group run without a subcommand on a terminal opens its interactive menu. On a
non-terminal (pipes, scripts) it prints help, so flag-driven usage stays scriptable.

### Templates

| Name | Protocol | Notes |
|---|---|---|
| `vless-reality` *(default)* | vless + REALITY (vision) | best against active probing; keys auto-generated |
| `vmess-ws` | vmess + websocket | CDN-friendly |
| `vmess-tcp` | vmess over tcp | |
| `socks` | SOCKS5 (loopback) | local proxy |

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
cmd/emx/          cobra CLI, bubbletea TUI, gRPC client
core/xray/        OS-agnostic: models, store, link parser, config Generate, dialer, keygen, templates
daemon/           supervisor (Reconcile / SyncDialerMembers), watchdog, xray api, gRPC server, scheduler
api/              emx.proto + generated stubs
internal/         paths (XDG), xraybin (go:embed)
scripts/          fetch-xray.sh
```

---

## License

[MIT](LICENSE)
