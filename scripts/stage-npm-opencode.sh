#!/usr/bin/env bash
set -euo pipefail

ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
PKG="$ROOT/npm/opencode"

if [ "${1:-}" = "--build" ]; then
  make -C "$ROOT" build-npm-platforms
fi

DARWIN_PKG="$ROOT/npm/platform/darwin-arm64"
LINUX_PKG="$ROOT/npm/platform/linux-x64"
WIN_X64_PKG="$ROOT/npm/platform/win32-x64"
WIN_ARM64_PKG="$ROOT/npm/platform/win32-arm64"

node -e '
  const fs = require("fs")
  // Last argv is bin/platform.js (scanned as TEXT); everything before it is a package.json.
  const argv = process.argv.slice(1)
  const platformJsPath = argv[argv.length - 1]
  const paths = argv.slice(0, -1)
  const packages = paths.map((file) => JSON.parse(fs.readFileSync(file, "utf8")))
  const versions = new Set(packages.map((pkg) => pkg.version))
  if (versions.size !== 1) throw new Error(`npm package versions differ: ${[...versions].join(", ")}`)
  const main = packages[0]
  for (const platform of packages.slice(1)) {
    if (main.optionalDependencies?.[platform.name] !== platform.version) {
      throw new Error(`${platform.name} optional dependency must equal ${platform.version}`)
    }
  }
  // Every platform package must ALSO be reachable from the resolver, or a published
  // binary is dead weight: platformPackage() is what turns a platform into a package
  // name, and a package absent from that map can never be resolved at runtime. This
  // catches the half-done case (package published, map not updated) at stage time.
  // Read as TEXT, not JSON — __dirname is undefined under `node -e`, hence the argv hand-off.
  const platformJs = fs.readFileSync(platformJsPath, "utf8")
  for (const platform of packages.slice(1)) {
    if (!platformJs.includes(platform.name)) {
      throw new Error(`${platform.name} is not listed in npm/opencode/bin/platform.js PACKAGES`)
    }
  }
' "$PKG/package.json" "$DARWIN_PKG/package.json" "$LINUX_PKG/package.json" "$WIN_X64_PKG/package.json" "$WIN_ARM64_PKG/package.json" "$PKG/bin/platform.js"

rm -rf "$PKG/dist" "$PKG/prompts" "$PKG/assets" \
  "$DARWIN_PKG/bin" "$LINUX_PKG/bin" "$WIN_X64_PKG/bin" "$WIN_ARM64_PKG/bin"
mkdir -p "$DARWIN_PKG/bin" "$LINUX_PKG/bin" "$WIN_X64_PKG/bin" "$WIN_ARM64_PKG/bin"

test -f "$ROOT/bin/witness-darwin-arm64" || { echo "missing bin/witness-darwin-arm64; run: make build-npm-platforms" >&2; exit 1; }
test -f "$ROOT/bin/witness-linux-amd64" || { echo "missing bin/witness-linux-amd64; run: make build-npm-platforms" >&2; exit 1; }
test -f "$ROOT/bin/witness-windows-amd64.exe" || { echo "missing bin/witness-windows-amd64.exe; run: make build-npm-platforms" >&2; exit 1; }
test -f "$ROOT/bin/witness-windows-arm64.exe" || { echo "missing bin/witness-windows-arm64.exe; run: make build-npm-platforms" >&2; exit 1; }

cp "$ROOT/bin/witness-darwin-arm64" "$DARWIN_PKG/bin/witness"
cp "$ROOT/bin/witness-linux-amd64" "$LINUX_PKG/bin/witness"
# .exe, matching binName() in npm/opencode/bin/platform.js. The suffix is load-bearing:
# both consumers gate the resolved path on existsSync, so a POSIX-named Windows binary
# resolves to nothing and the plugin silently captures zero.
cp "$ROOT/bin/witness-windows-amd64.exe" "$WIN_X64_PKG/bin/witness.exe"
cp "$ROOT/bin/witness-windows-arm64.exe" "$WIN_ARM64_PKG/bin/witness.exe"

cp -R "$ROOT/prompts" "$PKG/prompts"

chmod +x "$PKG/bin/witness.js"
chmod +x "$PKG/bin/download-model.js"
chmod +x "$DARWIN_PKG/bin/witness" "$LINUX_PKG/bin/witness"
# The Windows binaries get the exec bit too: npm preserves mode, and a checkout that
# later runs these under WSL or a POSIX shell would otherwise hit EACCES.
chmod +x "$WIN_X64_PKG/bin/witness.exe" "$WIN_ARM64_PKG/bin/witness.exe"

echo "staged npm packages in $PKG and $ROOT/npm/platform"
