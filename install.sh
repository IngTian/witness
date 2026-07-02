#!/usr/bin/env bash
# One-command install for claude-witness (working-copy / from-source).
# Builds the binary, fetches the embedding model once, and wires the Claude Code
# hooks + MCP server. Idempotent — safe to re-run after a `git pull`.
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")"

command -v go >/dev/null 2>&1 || { echo "error: 'go' not on PATH (need Go 1.25+)"; exit 1; }

BIN="bin/witness-$(go env GOOS)-$(go env GOARCH)"
echo "==> building $BIN"
CGO_ENABLED=0 go build -o "$BIN" ./cmd/witness

# fetch-model.sh is idempotent and size-verifying: it skips files already present
# and intact, and re-fetches a truncated one — so always defer to it.
echo "==> ensuring embedding model (~448MB, fetched once)"
./scripts/fetch-model.sh

echo "==> wiring hooks + MCP server"
"$BIN" install

echo
echo "Installed. Verify with:  GOMLX_BACKEND=go $BIN doctor"
echo "Then restart Claude Code (or open /hooks) so the hooks load."
