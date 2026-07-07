# @witness-ai/opencode

OpenCode plugin for witness. It captures OpenCode message events, reconciles OpenCode's local session database on idle, and kicks the witness background distillation worker.

## Install

Install the package, then add the plugin to your OpenCode config:

```sh
npm install -g @witness-ai/opencode
```

```json
{
  "$schema": "https://opencode.ai/config.json",
  "plugin": ["@witness-ai/opencode"]
}
```

The package includes the `witness` CLI, bundled platform binaries, and prompts. The plugin uses the bundled binary first, then falls back to `WITNESS_BIN` or `witness` from `PATH`.

If you want to force a different binary, set `WITNESS_BIN` before starting OpenCode:

```sh
export WITNESS_BIN=/absolute/path/to/witness
```

You still need the OpenCode MCP server registered separately so agents can read the profile through the `witness` MCP tools.
