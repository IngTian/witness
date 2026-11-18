# Windows install

Windows has no guaranteed shell, so the install path differs from Unix: the binary and its assets are copied into `%LOCALAPPDATA%` and hooks are registered in exec form.

### Windows

Windows uses a self-contained zip instead of the shell installer (there is no
guaranteed shell to run the hook shim). Download `witness-v<version>-windows-amd64.zip`
(Intel/AMD) or `witness-v<version>-windows-arm64.zip` from the
[latest release](https://github.com/IngTian/witness/releases/latest) — each unpacks
to a `witness\` folder holding `witness.exe` and the embedding model. Then, from
inside that folder in PowerShell:

```powershell
.\witness.exe wire claude      # Claude Code
.\witness.exe wire opencode    # OpenCode
```

Either command copies the bundle into `%LOCALAPPDATA%\witness`, adds it to your user
PATH, and provisions the archive. The zip carries the prompt templates and the ~448MB
model alongside the exe; the binary resolves both relative to itself. Running both is
fine — the copy is idempotent, and they wire different editors.

What each wires, and why they differ: Claude Code spawns the hook itself, so `wire
claude` writes **exec-form hooks** (`{command: witness.exe, args: [...]}`) into
`settings.json`. OpenCode is the other way round — its *plugin* spawns witness, so
`wire opencode` bakes the installed `witness.exe` path into the plugin and the MCP
entry. Neither needs a shell or Git Bash.

To remove an editor integration: `witness.exe unwire claude` / `unwire opencode`
(strips the hooks/plugin + MCP entry). The copied files and PATH entry are left in
place, since the other integration may still be using them.

### OpenCode on Windows

OpenCode can be installed through multiple channels:

- **npm**: `npm install -g @opencode/cli` — install scripts must be enabled in your npm config.
- **Standalone zip**: `opencode-windows-x64.zip` from the OpenCode releases page — unpack and run
  `opencode.exe` directly; put its folder on your PATH (or set `WITNESS_OPENCODE_BIN`) so witness's
  own commands find it.
- **curl installer** (under Git Bash): the one-liner installer lands OpenCode in
  `%USERPROFILE%\.opencode\bin` and updates PATH for Git Bash only, but witness looks there anyway so
  the integration still works.
- **winget**: `winget install Anomaly.OpenCode` (once its manifest is accepted into winget's
  repository).
- **Desktop app**: downloads include a bundled CLI, but it is not added to PATH — the plugin hands
  witness its `opencode-cli.exe` path, so the integration works regardless.

**Git Bash / mintty is not a TTY** — `witness wire opencode` never prompts there and prints hints
instead. Run it in Windows Terminal, PowerShell or cmd to get the interactive runner and model prompt.

`bun install -g @opencode/cli` is not verified with witness. Its `opencode.exe` shim may not resolve
correctly. Stick to npm, the zip, the desktop app, or set `WITNESS_OPENCODE_BIN` to a known-good
binary.

`%ProgramData%\opencode` holds machine-wide OpenCode config and plugins for every user. It loads into
witness's private OpenCode serve too, and witness does not manage it.

`witness worker stop` ends the worker's `claude -p` and `opencode` children as well — they run in its
Job Object so Windows tears them down automatically.

The Claude runner runs `claude.exe` directly. npm's `claude.cmd` resolves to the exe it wraps, a bun
`.exe` shim runs as is, and other `.cmd`/`.bat` shims (pnpm, yarn) are refused: install natively
(`irm https://claude.ai/install.ps1 | iex`).

---

[← back to the README](../README.md)
