#!/usr/bin/env bash
# Thin shim: resolve the witness binary and dispatch a subcommand.
# Hooks pass the JSON event on stdin; the binary reads it.
#
# RECURSION GUARD (correctness-critical): the worker invokes `claude -p`, which
# is itself a Claude Code run that would fire these same hooks → capture its own
# log → spawn another worker → recurse forever. The worker sets WITNESS_WORKER=1
# on that subprocess; we short-circuit immediately when we see it.
set -euo pipefail

if [ "${WITNESS_WORKER:-}" = "1" ]; then
  # We are inside a witness-driven `claude -p`. Do nothing; consume stdin so the
  # producing process never blocks on a full pipe, and exit clean.
  cat >/dev/null 2>&1 || true
  exit 0
fi

PLUGIN_ROOT="${CLAUDE_PLUGIN_ROOT:-"$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"}"

# When run from a working copy (not an installed plugin) CLAUDE_PLUGIN_ROOT is
# unset, but the binary resolves prompts/, lenses/, and assets/ relative to it.
# Export it so a checkout "just works" without per-path WITNESS_* overrides.
export CLAUDE_PLUGIN_ROOT="$PLUGIN_ROOT"

# Locate the binary: prefer a bundled per-OS/arch build, else a PATH install,
# else fall back to `go run` for local development.
OS="$(uname -s | tr '[:upper:]' '[:lower:]')"
ARCH="$(uname -m)"
case "$ARCH" in
  x86_64) ARCH=amd64 ;;
  aarch64|arm64) ARCH=arm64 ;;
esac

BIN="${PLUGIN_ROOT}/bin/witness-${OS}-${ARCH}"
if [ ! -x "$BIN" ]; then
  if command -v witness >/dev/null 2>&1; then
    BIN="$(command -v witness)"
  elif command -v go >/dev/null 2>&1 && [ -f "${PLUGIN_ROOT}/go.mod" ]; then
    # Dev fallback. GOMLX_BACKEND=go keeps it pure-Go (no XLA/CGo).
    exec env GOMLX_BACKEND=go go -C "$PLUGIN_ROOT" run ./cmd/witness "$@"
  else
    # No binary and no way to build one. Capture must never break the session:
    # swallow stdin and exit success so the hook is a no-op.
    cat >/dev/null 2>&1 || true
    exit 0
  fi
fi

exec env GOMLX_BACKEND=go "$BIN" "$@"
