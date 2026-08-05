#!/bin/bash
# FRESH INSTALL race: `witness config set runner opencode` (SetRunner) vs the
# first-open default-lens AUTO-SEED (fires from ANY other command's store.Open).
# Both write config.toml unlocked. Count archives left with the default lens
# REGISTERED but NOT ENABLED and the one-shot marker already burned.
BIN=/tmp/wtest-bin
ROOT=$1
wedged=0; n=80
rm -rf "$ROOT"; mkdir -p "$ROOT"
for i in $(seq 1 $n); do
  H=$ROOT/a$i
  mkdir -p "$H"
  WITNESS_HOME=$H $BIN config set runner opencode >/dev/null 2>&1 &
  WITNESS_HOME=$H $BIN status >/dev/null 2>&1 &
  wait
  # Now re-open a few times: the one-shot must have a chance to self-heal.
  WITNESS_HOME=$H $BIN status >/dev/null 2>&1
  WITNESS_HOME=$H $BIN status >/dev/null 2>&1
  reg=$(WITNESS_HOME=$H $BIN lens list 2>/dev/null | grep -c 'default')
  en=$(grep -cE '^[[:space:]]*lens[[:space:]]*=[[:space:]]*default' "$H/config.toml" 2>/dev/null)
  if [ "$reg" -gt 0 ] && [ "$en" -eq 0 ]; then
    wedged=$((wedged+1)); echo "WEDGED: $H (default registered, NOT enabled)"
  fi
done
echo "archives=$n  permanently un-enabled default (distills NOTHING, doctor says ok)=$wedged"
