# @witness-ai/opencode

OpenCode plugin for witness. It reconciles OpenCode's local session database on startup and when a session goes idle, and starts witness distillation only through the CLI's laptop-friendly auto-start gate.

## Supported platforms

| Operating system | Architecture | Installed binary package |
| --- | --- | --- |
| macOS | Apple Silicon (`darwin/arm64`) | `@witness-ai/opencode-darwin-arm64` |
| Linux | x86-64 (`linux/x64`) | `@witness-ai/opencode-linux-x64` |

These are the only platforms supported by the npm distribution. macOS Intel, Linux ARM, and Windows are not supported. The CLI exits with a clear supported-platform message on those systems.

## Install

Add the npm plugin to your OpenCode config. OpenCode installs the package automatically with Bun on startup, and the plugin auto-registers `mcp.witness` if you have not already defined one:

```json
{
  "$schema": "https://opencode.ai/config.json",
  "plugin": ["@witness-ai/opencode"]
}
```

To test a prerelease, pin the plugin entry to the beta version instead of the unversioned package name:

```json
{
  "$schema": "https://opencode.ai/config.json",
  "plugin": ["@witness-ai/opencode@beta"]
}
```

Optional: install it globally if you also want a `witness` CLI on your shell `PATH`:

```sh
npm install -g @witness-ai/opencode
```

Optional: run the CLI ad hoc without a global install:

```sh
npm exec --yes --package=@witness-ai/opencode -- witness doctor
```

The main package includes the plugin, CLI wrapper, and prompts. npm installs one optional platform package containing the matching `witness` binary; it does not download binaries for other operating systems or architectures. The first embedding-model download is about 470MB into the witness data directory (`assets/e5-small`). `npm install` only installs the packages; the OpenCode plugin starts the model download when OpenCode next starts. Keep OpenCode running until the first download finishes. If OpenCode stops early, the plugin stops its downloader and retries later with bounded backoff. Set `WITNESS_BIN` to override the platform package binary.

After the download completes, verify the model, OpenCode runner, archive, and queue:

```sh
npm exec --yes --package=@witness-ai/opencode@beta -- witness doctor
npm exec --yes --package=@witness-ai/opencode@beta -- witness distill status
```

By default the model is downloaded from Hugging Face. A custom `WITNESS_MODEL_BASE_URL` must serve the same paths (`onnx/model.onnx` and `tokenizer.json`) and also set `WITNESS_MODEL_SHA256` plus `WITNESS_TOKENIZER_SHA256`.

Automatic distillation is batched by default: sessions are reconciled on startup and idle, while model work starts at most once every 10 minutes, drains the current queue, then exits so the embed model is not resident. Edit witness `config.toml` if you want manual-only behavior:

```toml
auto_distill = false
```

If you want to force a different binary, set `WITNESS_BIN` before starting OpenCode:

```sh
export WITNESS_BIN=/absolute/path/to/witness
```

If you already have your own `mcp.witness` entry, the plugin leaves it alone. The npm CLI deliberately does not support `witness install` / `witness uninstall`; those are source-checkout workflows, not the npm one.
