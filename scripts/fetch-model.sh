#!/usr/bin/env bash
# Fetch the multilingual-e5-small fp32 ONNX model + tokenizer into assets/e5-small.
# Run once after clone (or let install.sh call it). No Python — curl against the HF CDN.
#
# Robustness: each file downloads to a .part and is renamed into place ONLY after a
# size check passes, so an interrupted/partial download (or an HF LFS-pointer/error
# page served instead of the blob) never leaves a truncated file that later runs
# would treat as "present". The skip-guard checks size too, so an already-truncated
# file is re-fetched rather than trusted.
set -euo pipefail

DEST="${1:-$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)/assets/e5-small}"
REPO="intfloat/multilingual-e5-small"
BASE="https://huggingface.co/${REPO}/resolve/main"

mkdir -p "$DEST"
echo "Fetching e5-small into $DEST ..."

filesize() { wc -c < "$1" | tr -d ' '; }

# fetch <remote-path> <out-name> <min-bytes>
fetch() {
  local path="$1" out="$2" min="$3" dst="$DEST/$2" tmp="$DEST/$2.part"
  if [ -f "$dst" ] && [ "$(filesize "$dst")" -ge "$min" ]; then
    echo "  have $out"; return
  fi
  echo "  downloading $out ..."
  rm -f "$tmp"
  curl -fL --retry 3 "${BASE}/${path}" -o "$tmp"
  local got; got="$(filesize "$tmp")"
  if [ "$got" -lt "$min" ]; then
    rm -f "$tmp"
    echo "  ERROR: $out is only ${got} bytes (expected >= ${min}); download incomplete or not the real file" >&2
    exit 1
  fi
  mv -f "$tmp" "$dst"
  echo "  ok $out (${got} bytes)"
}

# Minimum sizes are a coarse integrity guard (real: ~470MB model, ~17MB tokenizer);
# they reject truncations and HTML/LFS-pointer responses without pinning exact bytes
# (which would break every install whenever the upstream model legitimately updates).
fetch "onnx/model.onnx" "model.onnx"     400000000
fetch "tokenizer.json"  "tokenizer.json"   1000000

echo "Done. ($(du -h "$DEST/model.onnx" | cut -f1) model)"
