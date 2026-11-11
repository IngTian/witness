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

# When run from a working copy CLAUDE_PLUGIN_ROOT is normally
# unset (the name is historical; see internal/bundle), but the binary resolves prompts/ and assets/ relative to it.
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
  # The PATH `witness` may be install.sh's ~/.local/bin launcher, which execs THIS shim:
  # with no bin/ build (make clean, git clean, an unmapped arch) the two re-exec each other
  # forever. WITNESS_VIA_SHIM marks the second entry, which falls through instead.
  if [ -z "${WITNESS_VIA_SHIM:-}" ] && command -v witness >/dev/null 2>&1; then
    BIN="$(command -v witness)"
    export WITNESS_VIA_SHIM=1
  elif command -v go >/dev/null 2>&1 && [ -f "${PLUGIN_ROOT}/go.mod" ]; then
    # Dev fallback for a checkout that was never built.
    exec go -C "$PLUGIN_ROOT" run ./cmd/witness "$@"
  elif [ -t 0 ]; then
    # A person typed this (hooks always pipe stdin): say why nothing happened rather than
    # block on `cat` waiting for a terminal EOF.
    echo "witness: no witness binary: run make build in ${PLUGIN_ROOT}" >&2
    exit 1
  else
    # No binary and no way to build one. Capture must never break the session:
    # swallow stdin and exit success so the hook is a no-op.
    cat >/dev/null 2>&1 || true
    exit 0
  fi
fi

exec "$BIN" "$@"
