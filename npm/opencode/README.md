# @witness-ai/opencode

OpenCode plugin for witness. It reconciles OpenCode's local session database on startup and at the end of every turn, and starts witness distillation only through the CLI's laptop-friendly auto-start gate.

## Supported platforms

| Operating system | Architecture | Installed binary package |
| --- | --- | --- |
| macOS | Apple Silicon (`darwin/arm64`) | `@witness-ai/opencode-darwin-arm64` |
| Linux | x86-64 (`linux/x64`) | `@witness-ai/opencode-linux-x64` |
| Windows | x86-64 (`win32/x64`) | `@witness-ai/opencode-win32-x64` |
| Windows | ARM64 (`win32/arm64`) | `@witness-ai/opencode-win32-arm64` |

These are the only platforms supported by the npm distribution. macOS Intel and Linux ARM are not supported; the CLI exits with a clear supported-platform message on those systems.

Requires OpenCode **2.2.7 or newer** (v1 is not supported; witness v0.13.0 was the last release for v1). On Windows, if you prefer a self-contained install with the embedding model already included — no first-run download — use the zip from the [latest release](https://github.com/IngTian/witness/releases/latest) instead.

## Install

Add the npm plugin to your OpenCode config. OpenCode installs the package automatically with Bun on startup, and the plugin auto-registers `mcp.witness` if you have not already defined one:

```json
{
  "$schema": "https://opencode.ai/config.json",
  "plugins": ["@witness-ai/opencode"]
}
```

To test a prerelease, pin an explicit version instead of the unversioned package name — e.g.
`"plugins": ["@witness-ai/opencode@0.9.1-beta.1"]`. Use a real published version; the `beta` dist-tag
is not kept current, so pinning it can silently install something several releases old.

Optional: install it globally if you also want a `witness` CLI on your shell `PATH`:

```sh
npm install -g @witness-ai/opencode
```

Optional: run the CLI ad hoc without a global install:

```sh
npm exec --yes --package=@witness-ai/opencode -- witness doctor
```

The main package includes the plugin, CLI wrapper, and prompts. npm installs one optional platform package containing the matching `witness` binary; it does not download binaries for other operating systems or architectures. The embedding model is not in the package: the plugin runs `witness model fetch` once each time OpenCode starts (~470 MB the first time, into the witness data directory's `assets/e5-small`; later starts only check it); `witness mcp` answers search once it lands, and the other tools work at once. Run as an MCP server alone (`npx @witness-ai/opencode mcp`, as the MCP Registry entry does), witness downloads the model itself in the background. `npx @witness-ai/opencode model fetch` downloads it in the foreground instead. Set `WITNESS_BIN` to override the platform package binary.

After the download completes, verify the model, OpenCode runner, archive, and queue:

```sh
npm exec --yes --package=@witness-ai/opencode -- witness doctor
npm exec --yes --package=@witness-ai/opencode -- witness status
```

> **Upgrade note:** Older witness releases could leave an OpenCode session whose agent or title is
> `witness-distill` when distillation was interrupted. Current releases never create them and skip
> any leftovers on import, so they are never captured; delete them from OpenCode if you want them gone.

By default the model is downloaded from Hugging Face. A custom `WITNESS_MODEL_BASE_URL` must serve the same paths (`onnx/model.onnx` and `tokenizer.json`) and also set `WITNESS_MODEL_SHA256` plus `WITNESS_TOKENIZER_SHA256`.

Automatic distillation is batched by default: sessions are reconciled on startup and at every turn end, while model work starts after a 5-minute quiet period once no session is running (each new turn resets it), drains the current queue, then exits so the embed model is not resident. Edit witness `config.toml` if you want manual-only behavior:

```toml
auto_distill = false
```

If you want to force a different binary, set `WITNESS_BIN` before starting OpenCode:

```sh
export WITNESS_BIN=/absolute/path/to/witness
```

If you already have your own `mcp.witness` entry, the plugin leaves it alone. The npm CLI deliberately refuses `witness wire` / `witness unwire`; editor wiring is a source-checkout workflow, and here the plugin registers itself. `witness install` (archive provisioning) works as usual.
