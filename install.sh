#!/usr/bin/env bash
# One-command install for claude-witness (working-copy / from-source).
# Builds the binary, fetches the embedding model once, and wires the Claude Code
# hooks + MCP server. Idempotent — safe to re-run after a `git pull`.
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")"

command -v go >/dev/null 2>&1 || { echo "error: 'go' not on PATH (need Go 1.25+)"; exit 1; }

BIN="bin/witness-$(go env GOOS)-$(go env GOARCH)"
echo "==> building $BIN"
CGO_ENABLED=0 go build -o "$BIN" ./cmd/witness

# fetch-model.sh is idempotent and size-verifying: it skips files already present
# and intact, and re-fetches a truncated one — so always defer to it.
echo "==> ensuring embedding model (~448MB, fetched once)"
./scripts/fetch-model.sh

echo "==> wiring hooks + MCP server"
"$BIN" install

# Optionally expose a `witness` command on PATH for the human subcommands
# (profile, doctor, lens, cleanup). Hooks + MCP don't need this — they invoke the
# shim directly — but it makes `witness <cmd>` work as the docs show. The launcher
# execs the shim by its real path so GOMLX_BACKEND + CLAUDE_PLUGIN_ROOT resolve.
ROOT="$PWD"
if command -v witness >/dev/null 2>&1; then
  echo "==> 'witness' already on PATH ($(command -v witness)) — leaving it"
else
  reply=n
  if [ -t 0 ]; then
    printf "==> add a 'witness' command to your PATH (~/.local/bin)? [Y/n] "
    read -r reply || reply=n
  fi
  case "${reply:-Y}" in
    [Nn]*) echo "    skipped — run the CLI via ./hooks/witness.sh <cmd>" ;;
    *)
      mkdir -p "$HOME/.local/bin"
      cat > "$HOME/.local/bin/witness" <<EOF
#!/usr/bin/env bash
# Launcher for the claude-witness CLI — execs the plugin shim by its real path.
exec "$ROOT/hooks/witness.sh" "\$@"
EOF
      chmod +x "$HOME/.local/bin/witness"
      echo "    installed $HOME/.local/bin/witness"
      case ":$PATH:" in
        *":$HOME/.local/bin:"*) : ;;
        *) echo "    note: ~/.local/bin is not on your PATH — add: export PATH=\"\$HOME/.local/bin:\$PATH\"" ;;
      esac
      ;;
  esac
fi

echo
echo "Installed. Verify with:  witness doctor   (or: GOMLX_BACKEND=go $BIN doctor)"
echo "Then restart Claude Code (or open /hooks) so the hooks load."
