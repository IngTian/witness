# OpenCode support

Setup, the isolated-runtime model, and the npm install path for OpenCode users.

### OpenCode support

OpenCode support has two pieces:

- A plugin reconciles OpenCode's SQLite DB on startup and when a session goes idle, then asks the
  laptop-friendly auto-start gate to distill when allowed. From-source installs write a
  local plugin to `~/.config/opencode/plugins/witness.js`; published installs can use the npm plugin
  `@witness-ai/opencode`. (That path is the same on Windows — OpenCode resolves its config as
  `XDG_CONFIG_HOME` else `~/.config`, with no Windows-specific branch, so the plugin lands in
  `%USERPROFILE%\.config\opencode\plugins\`.)
- An OpenCode MCP entry named `witness` launches the same MCP server as Claude Code, exposing
  `get_profile`, `get_facets`, `search_observations`, `record_observation`, and
  `delete_observation`.

The npm package ships the OpenCode plugin, a `witness` CLI shim, prebuilt witness binaries, and prompts.
The config-only path is the default: add the plugin to `~/.config/opencode/opencode.json`, and OpenCode
installs it automatically with Bun on startup. If `mcp.witness` is absent, the plugin auto-registers it
for you.

The npm package supports OpenCode integration and general CLI usage (`witness profile`, `ingest`, etc.).
Editor wiring (`witness wire` / `unwire`) is a source-checkout command — npm users configure the plugin
directly in `opencode.json` and the plugin auto-registers MCP. Archive provisioning and record ingestion
(`witness install`, `witness ingest`) work normally with the npm package.

The npm distribution supports exactly these platforms:

| Operating system | Architecture | npm platform package |
| --- | --- | --- |
| macOS | Apple Silicon (`darwin/arm64`) | `@witness-ai/opencode-darwin-arm64` |
| Linux | x86-64 (`linux/x64`) | `@witness-ai/opencode-linux-x64` |

macOS Intel, Linux ARM, and Windows are not supported by the npm distribution. Each binary is
published as an optional platform package, so npm installs only the binary for the current machine.

```json
{
  "$schema": "https://opencode.ai/config.json",
  "plugin": ["@witness-ai/opencode"]
}
```

To test the current prerelease without replacing `latest`, pin the plugin entry:

```json
{
  "$schema": "https://opencode.ai/config.json",
  "plugin": ["@witness-ai/opencode@beta"]
}
```

Optional: install it globally if you also want a `witness` command on your shell `PATH`:

```sh
npm install -g @witness-ai/opencode
```

Optional: run it ad hoc without a global install:

```sh
npm exec --yes --package=@witness-ai/opencode -- witness doctor
```

The main npm package contains the plugin, CLI wrapper, and prompts; the matching optional platform package
contains one binary. The first embedding-model download is about 470MB. Installing the packages does not
start that download: the plugin starts it when OpenCode next runs. Keep OpenCode running until the first
download finishes. The plugin owns the downloader, stops it on shutdown, and retries later with bounded
backoff.
If you already have your own `mcp.witness` config, the plugin leaves it untouched. The npm wrapper does
not support `witness wire` / `witness unwire`; those commands are for source-checkout editor wiring.
Custom model mirrors must provide `WITNESS_MODEL_SHA256` and `WITNESS_TOKENIZER_SHA256` alongside
`WITNESS_MODEL_BASE_URL`.

After the first download, verify the model, OpenCode runner, archive, and queue:

```sh
npm exec --yes --package=@witness-ai/opencode@beta -- witness doctor
npm exec --yes --package=@witness-ai/opencode@beta -- witness status
```

> **Upgrade note:** Older witness releases could leave an OpenCode session whose agent or title is
> `witness-distill` when distillation was interrupted. Current releases no longer create or filter
> those sessions in OpenCode's user database. Before the first import after upgrading, remove any
> leftover `witness-distill` sessions with OpenCode's supported session-management tools so they are
> not captured as normal archive data.

The npm package lives in [`npm/opencode`](../npm/opencode). Stage prebuilt binaries and prompts before publishing:

```sh
make npm-opencode-package
npm publish ./npm/platform/darwin-arm64 --access public --tag beta
npm publish ./npm/platform/linux-x64 --access public --tag beta
(cd npm/opencode && npm publish --access public --ignore-scripts --tag beta)
```

Configure npm Trusted Publishing separately for the main package and both platform packages before using
the release workflow. The npm package page renders [`npm/opencode/README.md`](../npm/opencode/README.md);
the workflow verifies after publishing that npm identifies it as the package README and that the published
tarball contains it.

For the first platform-package release only, publish both platform packages manually before creating the
GitHub Release, then configure their Trusted Publishers on npm. npm requires a package to exist before its
package-level Trusted Publisher can be configured. The release workflow is idempotent: it skips an already
published package version, publishes any missing platform versions first, then publishes the main package.

Manual verification path:

```sh
witness lens register math prompts/lens/example   # optional: register an extra lens
witness lens enable math
witness import --agent opencode    # reconciles ~/.local/share/opencode/opencode.db and returns
witness status                    # watch non-blocking distillation progress
witness worker review              # forces L2 facets + L4 markdown profiles (hidden cmd)
witness profile opencode           # per-lens L4 report
witness profile                    # unified L4 report
```

---

[← back to the README](../README.md)
