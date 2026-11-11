#!/usr/bin/env bash
# Checks every version a release publishes against ONE expected version, plus the MCP Registry's
# 100-char description cap. The release job passes the tag (vX.Y.Z without the v); PR CI passes
# nothing, so the npm package's own version is the reference. Running it on PRs is the point: a
# manifest bumped separately (issue #54) or a description grown past 100 now fails before a tag
# exists, not on the release event after the tag is cut.
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/.."

tag="${1:-$(node -p "require('./npm/opencode/package.json').version")}"
check() {
  local label="$1" got="$2"
  if [ "$tag" != "$got" ]; then
    echo "release version ($tag) != $label version ($got)" >&2
    echo "bump $label to $tag (or tag v$got) and re-release." >&2
    exit 1
  fi
}
check "npm/opencode/package.json" "$(node -p "require('./npm/opencode/package.json').version")"
# server.json feeds the MCP Registry publish. Both its own version and the npm package reference
# must track the tag, or the registry would reject a stale version or point at the wrong package.
check "server.json" "$(node -p "require('./server.json').version")"
check "server.json packages[0]" "$(node -p "require('./server.json').packages[0].version")"

# The MCP Registry rejects a description over 100 characters with a 422 — AFTER npm has already
# published, so the run fails with the packages live and the registry entry missing. That happened
# on v0.7.8 (186 chars).
desc_len="$(node -p "require('./server.json').description.length")"
if [ "$desc_len" -gt 100 ]; then
  echo "server.json description is $desc_len chars; the MCP Registry rejects >100" >&2
  node -p "require('./server.json').description" >&2
  exit 1
fi
