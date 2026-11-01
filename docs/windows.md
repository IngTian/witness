# Windows install

Windows has no guaranteed shell, so the install path differs from Unix: the binary and its assets are copied into `%LOCALAPPDATA%` and hooks are registered in exec form.

### Windows

Windows uses a self-contained zip instead of the shell installer (there is no
guaranteed shell to run the hook shim). Download `witness-windows-amd64.zip`
(Intel/AMD) or `witness-windows-arm64.zip` from the releases page — each unpacks
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

---

[← back to the README](../README.md)
