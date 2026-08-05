#!/bin/bash
# Can a normal CLI op leave a 0-BYTE config.toml that EnsureConfigFile then refuses to
# refill? rewriteEnabledLens joins kept-lines; when the only content is the lens line
# being dropped, out == "" and writeAtomic publishes an EMPTY file.
set -u
H=$1
BIN=/tmp/wtest-bin
rm -rf "$H"; mkdir -p "$H"
printf 'lens = math\n' > "$H/config.toml"
echo "before: $(wc -c < "$H/config.toml") bytes"
WITNESS_HOME=$H WITNESS_NO_DEFAULT_LENS=1 $BIN lens disable math
echo "after lens disable: $(wc -c < "$H/config.toml") bytes"
WITNESS_HOME=$H WITNESS_NO_DEFAULT_LENS=1 $BIN config path >/dev/null
echo "after another Open (EnsureConfigFile ran): $(wc -c < "$H/config.toml") bytes"
