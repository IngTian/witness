# @witness-ai/opencode

OpenCode plugin for witness. It captures OpenCode message events, reconciles OpenCode's local session database on idle, and starts witness distillation only through the CLI's laptop-friendly auto-start gate.

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

The package includes the `witness` CLI, bundled platform binaries, and prompts. The OpenCode plugin starts the embedding model download into the witness data directory (`assets/e5-small`) while OpenCode is running. If OpenCode stops before the download finishes, the plugin stops its downloader. The plugin uses the bundled binary first, then falls back to `WITNESS_BIN` or `witness` from `PATH`.

By default the model is downloaded from Hugging Face. If that is slow or blocked, set `WITNESS_MODEL_BASE_URL` to a mirror that serves the same paths (`onnx/model.onnx` and `tokenizer.json`) before installing or starting OpenCode.

Automatic distillation is batched by default: raw capture is immediate, but model work starts at most once every 10 minutes, drains the current queue, then exits so the embed model is not resident. Edit witness `config.toml` if you want manual-only behavior:

```toml
auto_distill = false
```

If you want to force a different binary, set `WITNESS_BIN` before starting OpenCode:

```sh
export WITNESS_BIN=/absolute/path/to/witness
```

You still need the OpenCode MCP server registered separately so agents can read the profile through the `witness` MCP tools.
