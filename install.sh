#!/usr/bin/env bash
# One-command install for claude-witness (working-copy / from-source).
# Builds the binary, fetches the embedding model once, and wires the Claude Code
# hooks + MCP server. Idempotent — safe to re-run after a `git pull`.
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")"
TARGET="${1:-claude}"

# --- pretty output (decorative only; degrades to plain text when not a TTY) ----
if [ -t 1 ]; then
  B=$(printf '\033[1m'); D=$(printf '\033[2m'); G=$(printf '\033[32m')
  C=$(printf '\033[36m'); Y=$(printf '\033[33m'); R=$(printf '\033[31m'); X=$(printf '\033[0m')
else
  B=""; D=""; G=""; C=""; Y=""; R=""; X=""
fi
TOTAL=4
step() { printf '\n%s%s[%s/%s]%s %s%s%s\n' "$C" "$B" "$1" "$TOTAL" "$X" "$B" "$2" "$X"; }
ok()   { printf '      %s✓%s %s\n' "$G" "$X" "$1"; }
info() { printf '      %s%s%s\n' "$D" "$1" "$X"; }
die()  { printf '\n%s✗ %s%s\n' "$R" "$1" "$X" >&2; exit 1; }

printf '\n%s╭───────────────────────────────────────────╮%s\n' "$C" "$X"
printf '%s│%s  %sclaude-witness%s · installer               %s│%s\n' "$C" "$X" "$B" "$X" "$C" "$X"
printf '%s│%s  %slet Claude Code witness your growth%s      %s│%s\n' "$C" "$X" "$D" "$X" "$C" "$X"
printf '%s╰───────────────────────────────────────────╯%s\n' "$C" "$X"

# --- [1/4] preflight ----------------------------------------------------------
step 1 "Checking prerequisites"
command -v go >/dev/null 2>&1 || die "'go' not on PATH (need Go 1.25+)"
ok "go $(go version | awk '{print $3}' | sed 's/^go//')"

# --- [2/4] build --------------------------------------------------------------
BIN="bin/witness-$(go env GOOS)-$(go env GOARCH)"
step 2 "Building the binary"
info "$BIN — one pure-Go binary (CGO_ENABLED=0)"
CGO_ENABLED=0 go build -o "$BIN" ./cmd/witness
ok "built $BIN"

# --- [3/4] embedding model ----------------------------------------------------
step 3 "Ensuring the embedding model"
info "~448MB, fetched once; skipped if already present and intact"
./scripts/fetch-model.sh
ok "model ready"

# --- [4/4] wire into the selected agent runtime --------------------------------
case "$TARGET" in
  claude)   LABEL="Claude Code hooks + MCP server" ;;
  opencode) LABEL="OpenCode plugin + MCP server" ;;
  all)      LABEL="Claude Code + OpenCode integrations" ;;
  *) die "unknown install target '$TARGET' (want claude, opencode, or all)" ;;
esac
step 4 "Wiring $LABEL"
"$BIN" install "$TARGET"
ok "$LABEL registered"

# --- optional: put `witness` on PATH for the human subcommands ----------------
# (profile, doctor, lens, cleanup). Hooks + MCP invoke the shim directly and don't
# need this; the launcher execs the shim by its real path so GOMLX_BACKEND +
# CLAUDE_PLUGIN_ROOT resolve. Skipped non-interactively / when already on PATH.
ROOT="$PWD"
if command -v witness >/dev/null 2>&1; then
  info "witness already on PATH ($(command -v witness)) — leaving it"
else
  reply=n
  if [ -t 0 ]; then
    printf '\n      add a %switness%s command to your PATH (~/.local/bin)? [Y/n] ' "$B" "$X"
    read -r reply || reply=n
  fi
  case "${reply:-Y}" in
    [Nn]*) info "skipped — run the CLI via ./hooks/witness.sh <cmd>" ;;
    *)
      mkdir -p "$HOME/.local/bin"
      cat > "$HOME/.local/bin/witness" <<EOF
#!/usr/bin/env bash
# Launcher for the claude-witness CLI — execs the plugin shim by its real path.
exec "$ROOT/hooks/witness.sh" "\$@"
EOF
      chmod +x "$HOME/.local/bin/witness"
      ok "installed $HOME/.local/bin/witness"
      case ":$PATH:" in
        *":$HOME/.local/bin:"*) : ;;
        *) info "note: ~/.local/bin isn't on PATH — add: export PATH=\"\$HOME/.local/bin:\$PATH\"" ;;
      esac
      ;;
  esac
fi

# --- done ---------------------------------------------------------------------
printf '\n%s%s✓ Installed.%s\n' "$G" "$B" "$X"
printf '  %switness doctor%s   %s# verify (or: GOMLX_BACKEND=go %s doctor)%s\n' "$B" "$X" "$D" "$BIN" "$X"
case "$TARGET" in
  claude)   printf '  %sthen restart Claude Code (or open /hooks) so the hooks load%s\n\n' "$D" "$X" ;;
  opencode) printf '  %sthen restart OpenCode so the plugin and MCP server load%s\n\n' "$D" "$X" ;;
  all)      printf '  %sthen restart Claude Code and OpenCode so integrations load%s\n\n' "$D" "$X" ;;
esac
