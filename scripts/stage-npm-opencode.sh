#!/usr/bin/env bash
set -euo pipefail

ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
PKG="$ROOT/npm/opencode"

if [ "${1:-}" = "--build" ]; then
  make -C "$ROOT" build-npm-platforms
fi

DARWIN_PKG="$ROOT/npm/platform/darwin-arm64"
LINUX_PKG="$ROOT/npm/platform/linux-x64"

node -e '
  const fs = require("fs")
  const paths = process.argv.slice(1)
  const packages = paths.map((file) => JSON.parse(fs.readFileSync(file, "utf8")))
  const versions = new Set(packages.map((pkg) => pkg.version))
  if (versions.size !== 1) throw new Error(`npm package versions differ: ${[...versions].join(", ")}`)
  const main = packages[0]
  for (const platform of packages.slice(1)) {
    if (main.optionalDependencies?.[platform.name] !== platform.version) {
      throw new Error(`${platform.name} optional dependency must equal ${platform.version}`)
    }
  }
' "$PKG/package.json" "$DARWIN_PKG/package.json" "$LINUX_PKG/package.json"

rm -rf "$PKG/dist" "$PKG/prompts" "$PKG/assets" "$DARWIN_PKG/bin" "$LINUX_PKG/bin"
mkdir -p "$DARWIN_PKG/bin" "$LINUX_PKG/bin"

test -f "$ROOT/bin/witness-darwin-arm64" || { echo "missing bin/witness-darwin-arm64; run: make build-npm-platforms" >&2; exit 1; }
test -f "$ROOT/bin/witness-linux-amd64" || { echo "missing bin/witness-linux-amd64; run: make build-npm-platforms" >&2; exit 1; }

cp "$ROOT/bin/witness-darwin-arm64" "$DARWIN_PKG/bin/witness"
cp "$ROOT/bin/witness-linux-amd64" "$LINUX_PKG/bin/witness"

cp -R "$ROOT/prompts" "$PKG/prompts"

chmod +x "$PKG/bin/witness.js"
chmod +x "$PKG/bin/download-model.js"
chmod +x "$DARWIN_PKG/bin/witness" "$LINUX_PKG/bin/witness"

echo "staged npm packages in $PKG and $ROOT/npm/platform"
