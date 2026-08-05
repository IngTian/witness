#!/bin/bash
# Deterministic replay of the auto-seed vs config-set lost update on a FRESH install.
set -u
H=$1
BIN=/tmp/wtest-bin
rm -rf "$H"; mkdir -p "$H"

echo "=== 1. fresh archive: first-open auto-seed registers + ENABLES default, burns the one-shot ==="
WITNESS_HOME=$H $BIN lens list | head -4
echo "lens= lines in config.toml: $(grep -cE '^[[:space:]]*lens[[:space:]]*=' "$H/config.toml")"

echo
echo "=== 2. a CONCURRENT 'config set runner opencode' whose read predated the seed's rename"
echo "       publishes its stale copy (no lens line) + its runner line ==="
python3 - "$H/config.toml" <<'PY'
import sys, re
p = sys.argv[1]
lines = open(p).read().split("\n")
out = [l for l in lines if not re.match(r'^\s*lens\s*=', l)]
out.append('runner = "opencode"')
open(p, "w").write("\n".join(out) + "\n")
PY
echo "lens= lines now: $(grep -cE '^[[:space:]]*lens[[:space:]]*=' "$H/config.toml")"

echo
echo "=== 3. re-open the archive repeatedly — can anything self-heal? ==="
for i in 1 2 3; do WITNESS_HOME=$H $BIN status >/dev/null 2>&1; done
WITNESS_HOME=$H $BIN lens list
echo
echo "=== 4. doctor's verdict ==="
WITNESS_HOME=$H $BIN doctor 2>&1 | sed -n '1,2p;12,16p'
