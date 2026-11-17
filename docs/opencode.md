# OpenCode support

From-source wiring and the isolated-runtime model. **Installing from npm** (`@witness-ai/opencode`,
supported platforms, first-run model download) is documented in the
[npm package README](../npm/opencode/README.md).

> **OpenCode v2.2.7+ only; v0.13.0 is the last release for OpenCode v1.** `witness doctor`,
> `witness wire opencode` and the runner refuse OpenCode below 2.2.7 and all of v1; `witness import`
> refuses a v1 database. Above the newest CI-tested minor (2.3.x), `witness doctor` warns and witness
> keeps working. Run `opencode upgrade --major` to upgrade v1 to v2.

### OpenCode support

OpenCode support has two pieces:

- A plugin imports each session when a turn ends (succeeded, failed or interrupted) and syncs once
  when OpenCode starts; `wire` writes it atomically and OpenCode picks it up live. From a source
  checkout, `witness wire opencode` writes a local plugin to
  `~/.config/opencode/plugins/witness.js`; published installs use the npm plugin
  `@witness-ai/opencode` instead. (That path is the same on Windows — OpenCode resolves its config
  as `XDG_CONFIG_HOME` else `~/.config`, with no Windows-specific branch, so the plugin lands in
  `%USERPROFILE%\.config\opencode\plugins\`.)
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

The private serve has its own config dir, database, state and cache, but keeps your real `HOME` (for
ambient cloud credentials). Machine-wide OpenCode config in `/etc/opencode`,
`/Library/Application Support/opencode` (macOS) and `%ProgramData%\opencode` (Windows) loads into it
too; these are outside witness's control and are set by admin policy. The private serve still
discovers skills under `~/.agents/skills` at startup, but witness denies every tool so the model
cannot use them.

The runner can use free `opencode/*` models, API keys exported as environment variables
(`OPENAI_API_KEY`, `ANTHROPIC_API_KEY`, `AZURE_API_KEY`, `AWS_BEARER_TOKEN_BEDROCK`,
`OPENCODE_API_KEY` for paid Zen), and ambient cloud credentials (`AWS_PROFILE` / `AWS_REGION` /
`~/.aws` for Bedrock). It never uses `opencode auth login`, ChatGPT or other OAuth sign-ins, or
providers defined in your `opencode.json`. witness never reads or copies a credential. Keys exported
in your shell profile work for terminal-launched agents. GUI apps don't read it: use
`launchctl setenv` on macOS, or a user environment variable on Windows, so the worker inherits the
key. Set the stage model with `witness config set mine_model <provider/model>` (`review_model` falls
back to it); witness never picks one.

OpenCode's ChatGPT sign-in is an OAuth login, so witness's OpenCode runner cannot use it.
ChatGPT-plan users can use the separate [Codex runner](codex-runner.md).

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
