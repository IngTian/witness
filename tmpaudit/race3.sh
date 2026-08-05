#!/bin/bash
# REAL multi-process race on a FRESH archive, WITHOUT WITNESS_NO_DEFAULT_LENS:
#   P1: witness config set runner opencode   (SetRunner -> setConfigKey RMW)
#   P2: witness status                       (store.Open -> auto-seed -> EnableLens RMW)
# Then re-open 3x (the one-shot's only chance to self-heal) and check the END state:
# is the default lens left registered-but-DISABLED (=> nothing is EVER distilled)?
set -u
BIN=/tmp/wtest-bin
ROOT=$1
rm -rf "$ROOT"; mkdir -p "$ROOT"
n=${2:-120}
wedged=0; lostRunner=0
count() { grep -cE "$1" "$2" 2>/dev/null | head -1 || echo 0; }
for i in $(seq 1 "$n"); do
  H=$ROOT/a$i; mkdir -p "$H"
  WITNESS_HOME=$H $BIN config set runner opencode >/dev/null 2>&1 &
  WITNESS_HOME=$H $BIN status >/dev/null 2>&1 &
  wait
  for _ in 1 2 3; do WITNESS_HOME=$H $BIN status >/dev/null 2>&1; done
  reg=0; [ -f "$H/lenses/default/extract.md" ] && reg=1
  en=$(count '^[[:space:]]*lens[[:space:]]*=[[:space:]]*default' "$H/config.toml")
  run=$(count '^[[:space:]]*runner[[:space:]]*=' "$H/config.toml")
  en=${en:-0}; run=${run:-0}
  if [ "$run" = "0" ]; then lostRunner=$((lostRunner+1)); fi
  if [ "$reg" = "1" ] && [ "$en" = "0" ]; then
    wedged=$((wedged+1)); echo "WEDGED $H"
  fi
done
echo "archives=$n  default registered-but-DISABLED forever=$wedged  runner line lost=$lostRunner"
