# @witness-ai/opencode

OpenCode plugin for witness. It captures OpenCode message events, reconciles OpenCode's local session database on idle, and kicks the witness background distillation worker.

## Install

Install the `witness` CLI first, then add the npm plugin to your OpenCode config:

```json
{
  "$schema": "https://opencode.ai/config.json",
  "plugin": ["@witness-ai/opencode"]
}
```

The plugin invokes `witness` from `PATH`. If your binary is elsewhere, set `WITNESS_BIN` before starting OpenCode:

```sh
export WITNESS_BIN=/absolute/path/to/witness
```

You still need the OpenCode MCP server registered separately so agents can read the profile through the `witness` MCP tools.
