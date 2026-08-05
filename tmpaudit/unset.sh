#!/bin/bash
# `witness config unset runner` on an npm-OpenCode archive (no `claude` CLI):
# does it CLEAR the binding (restoring the WITNESS_RUNNER fallback) or PIN claude?
set -u
H=$1
BIN=/tmp/wtest-bin
rm -rf "$H"; mkdir -p "$H"
export WITNESS_HOME=$H WITNESS_NO_DEFAULT_LENS=1
echo "fresh (unbound), plugin env WITNESS_RUNNER=opencode:"
WITNESS_RUNNER=opencode $BIN config get runner
echo
echo "after 'config unset runner':"
$BIN config unset runner
grep -nE '^[[:space:]]*runner' "$H/config.toml"
WITNESS_RUNNER=opencode $BIN config get runner
