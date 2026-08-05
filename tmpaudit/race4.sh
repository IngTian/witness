#!/bin/bash
# The most COMMON schedule of all: two witness processes open the SAME FRESH archive at
# once (two Claude Code windows' capture hooks; the OpenCode plugin's `import` +
# `worker-kick`; `install` + a session hook). Both run store.Open ->
# EnsureConfigFile (template, tmp+rename) then the auto-seed -> EnableLens (RMW).
# If one's EnsureConfigFile Stat predates the other's rename, its TEMPLATE write lands
# AFTER the peer's EnableLens and erases the `lens = default` line — while the peer
# already stamped default_lens_migrated_v1a=done, so the one-shot never retries.
set -u
BIN=/tmp/wtest-bin
ROOT=$1
n=${2:-200}
rm -rf "$ROOT"; mkdir -p "$ROOT"
wedged=0
for i in $(seq 1 "$n"); do
  H=$ROOT/a$i; mkdir -p "$H"
  WITNESS_HOME=$H $BIN status >/dev/null 2>&1 &
  WITNESS_HOME=$H $BIN status >/dev/null 2>&1 &
  wait
  # give the one-shot every chance to self-heal
  for _ in 1 2 3; do WITNESS_HOME=$H $BIN status >/dev/null 2>&1; done
  if [ -f "$H/lenses/default/extract.md" ] && ! grep -qE '^[[:space:]]*lens[[:space:]]*=[[:space:]]*default' "$H/config.toml"; then
    wedged=$((wedged+1)); echo "WEDGED $H"
  fi
done
echo "fresh archives=$n  default registered but NEVER enabled=$wedged"
