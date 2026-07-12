#!/usr/bin/env bash
# Fetch the multilingual-e5-small fp32 ONNX model + tokenizer into assets/e5-small.
# Run once after clone (or let install.sh call it). No Python — curl against the HF CDN.
#
# Robustness: each file downloads to a .part and is renamed into place only after
# its size and pinned SHA-256 pass. Existing files are checked the same way.
set -euo pipefail

DEST="${1:-$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)/assets/e5-small}"
REPO="intfloat/multilingual-e5-small"
REVISION="614241f622f53c4eeff9890bdc4f31cfecc418b3"
BASE="https://huggingface.co/${REPO}/resolve/${REVISION}"
MODEL_SHA256="ca456c06b3a9505ddfd9131408916dd79290368331e7d76bb621f1cba6bc8665"
TOKENIZER_SHA256="0b44a9d7b51c3c62626640cda0e2c2f70fdacdc25bbbd68038369d14ebdf4c39"

mkdir -p "$DEST"
chmod 700 "$DEST"
echo "Fetching e5-small into $DEST ..."

filesize() { wc -c < "$1" | tr -d ' '; }
sha256() {
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$1" | awk '{print $1}'
  else
    shasum -a 256 "$1" | awk '{print $1}'
  fi
}

# fetch <remote-path> <out-name> <min-bytes> <sha256>
fetch() {
  local path="$1" out="$2" min="$3" want_sha="$4" dst="$DEST/$2" tmp="$DEST/$2.part"
  if [ -f "$dst" ] && [ "$(filesize "$dst")" -ge "$min" ]; then
    local have_sha; have_sha="$(sha256 "$dst")"
    if [ "$have_sha" = "$want_sha" ]; then
      echo "  have $out"; return
    fi
    rm -f "$dst"
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
  local got_sha; got_sha="$(sha256 "$tmp")"
  if [ "$got_sha" != "$want_sha" ]; then
    rm -f "$tmp"
    echo "  ERROR: $out sha256 mismatch: got ${got_sha}, want ${want_sha}" >&2
    exit 1
  fi
  mv -f "$tmp" "$dst"
  echo "  ok $out (${got} bytes)"
}

fetch "onnx/model.onnx" "model.onnx"   400000000 "$MODEL_SHA256"
fetch "tokenizer.json"  "tokenizer.json"   1000000 "$TOKENIZER_SHA256"

echo "Done. ($(du -h "$DEST/model.onnx" | cut -f1) model)"
