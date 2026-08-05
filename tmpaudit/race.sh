#!/bin/bash
# Race the FIRST-OPEN auto-seed (any command opens the store -> EnableLens("default"))
# against `witness config set runner opencode` (SetRunner). Both write config.toml
# unlocked. Count archives that end up with a lost update.
BIN=/tmp/wtest-bin
ROOT=$1
lostLens=0; lostRunner=0; boundNoLine=0; n=60
for i in $(seq 1 $n); do
  H=$ROOT/a$i
  rm -rf "$H"; mkdir -p "$H"
  WITNESS_HOME=$H $BIN config set runner opencode >/dev/null 2>&1 &
  P1=$!
  WITNESS_HOME=$H $BIN lens list >/dev/null 2>&1 &
  P2=$!
  wait $P1; wait $P2
  cfg="$H/config.toml"
  grep -qE '^[[:space:]]*lens[[:space:]]*=[[:space:]]*default' "$cfg" || lostLens=$((lostLens+1))
  grep -qE '^[[:space:]]*runner[[:space:]]*=' "$cfg" || lostRunner=$((lostRunner+1))
done
echo "archives=$n  lost 'lens = default' line=$lostLens   lost 'runner =' line=$lostRunner"
