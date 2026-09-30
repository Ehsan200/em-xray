#!/usr/bin/env bash
# Fetch the xray-core binary + geo data from the official XTLS/Xray-core GitHub
# release and drop them into internal/xraybin/assets/ for go:embed.
#
# Single set of assets is embedded, so this fetches ONE target triple. The build
# target decides which: default is linux-64 (the deploy target). For local
# testing on another OS, pass a TARGET that matches your host, e.g.
#   TARGET=macos-arm64 ./scripts/fetch-xray.sh
#
# Env:
#   XRAY_VERSION  release tag (default v26.3.27 — the verified version)
#   TARGET        release asset triple (default linux-64)
set -euo pipefail

XRAY_VERSION="${XRAY_VERSION:-v26.3.27}"
TARGET="${TARGET:-linux-64}"

repo_root="$(cd "$(dirname "$0")/.." && pwd)"
dest="$repo_root/internal/xraybin/assets"
asset="Xray-${TARGET}.zip"
url="https://github.com/XTLS/Xray-core/releases/download/${XRAY_VERSION}/${asset}"

echo ">> fetching ${asset} @ ${XRAY_VERSION}"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

curl -fsSL --retry 3 -o "$tmp/xray.zip" "$url"
unzip -q -o "$tmp/xray.zip" -d "$tmp/x"

mkdir -p "$dest"
# xray executable name is `xray` on unix, `xray.exe` on windows (unsupported here).
cp "$tmp/x/xray" "$dest/xray"
cp "$tmp/x/geoip.dat" "$dest/geoip.dat"
cp "$tmp/x/geosite.dat" "$dest/geosite.dat"
chmod +x "$dest/xray"

# Record what we embedded so `emx version` / the build are auditable.
printf 'version=%s\ntarget=%s\n' "$XRAY_VERSION" "$TARGET" > "$dest/VERSION"

echo ">> embedded xray ${XRAY_VERSION} (${TARGET}) into ${dest#$repo_root/}"
