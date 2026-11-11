# OpenCode support

From-source wiring and the isolated-runtime model. **Installing from npm** (`@witness-ai/opencode`,
supported platforms, first-run model download) is documented in the
[npm package README](../npm/opencode/README.md).

> **OpenCode v1 only (1.18 or newer, below 2.0).** OpenCode v2 (`@opencode/cli`) changed the
> server, plugin and storage formats witness depends on, so witness refuses it by name: `doctor`,
> `wire opencode` and `import --agent opencode` stop with "OpenCode v2 is not supported yet"
> instead of silently capturing nothing. v2 support is tracked in
> [#218](https://github.com/IngTian/witness/issues/218); until then use OpenCode v1.19.5
> (`npm i -g opencode-ai@1.19.5`).

### OpenCode support

OpenCode support has two pieces:

- A plugin reconciles OpenCode's SQLite DB on startup and when a session goes idle, then asks the
  laptop-friendly auto-start gate to distill when allowed. From a source checkout,
  `witness wire opencode` writes a local plugin to `~/.config/opencode/plugins/witness.js`;
  published installs use the npm plugin `@witness-ai/opencode` instead. (That path is the same on
  Windows — OpenCode resolves its config as `XDG_CONFIG_HOME` else `~/.config`, with no
  Windows-specific branch, so the plugin lands in `%USERPROFILE%\.config\opencode\plugins\`.)
- An OpenCode MCP entry named `witness` launches the same MCP server as Claude Code, exposing
  `get_profile`, `get_facets`, `search_observations`, `record_observation`, and
  `delete_observation`.

Editor wiring (`witness wire` / `unwire`) is a source-checkout command. npm users add the plugin to
`opencode.json` and the plugin auto-registers MCP; if you already have your own `mcp.witness`
entry, it is left untouched.

### The isolated runtime

witness reads your OpenCode database **read-only** to import sessions and never writes to it.
Distillation runs on a private `opencode serve` whose `XDG_DATA_HOME` and `OPENCODE_DB` point under
witness's own runtime directory, so its scratch sessions never appear in your OpenCode session list.
Your `auth.json` is copied (never linked) into that runtime so the private serve can reach your
providers; environment-backed providers (e.g. `ANTHROPIC_API_KEY`) work without it.

> **Upgrade note:** Older witness releases could leave an OpenCode session whose agent or title is
> `witness-distill` when distillation was interrupted. Current releases never create them and skip
> any leftovers on import, so they are never captured; delete them from OpenCode if you want them gone.

Manual verification path:

```sh
witness lens register math prompts/lens/example   # optional: register an extra lens
witness lens enable math
witness import --agent opencode    # reconciles ~/.local/share/opencode/opencode.db and returns
witness status                    # watch non-blocking distillation progress
witness worker review              # forces L2 facets + L4 markdown profiles
witness profile math               # per-lens L4 report
witness profile                    # unified L4 report
```

---

[← back to the README](../README.md)
