# witness — Let Claude Code and OpenCode witness your growth.

[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)
[![npm](https://img.shields.io/npm/v/@witness-ai/opencode?logo=npm&label=%40witness-ai%2Fopencode)](https://www.npmjs.com/package/@witness-ai/opencode)
![Go](https://img.shields.io/badge/Go-1.25-00ADD8?logo=go&logoColor=white)
![Single binary](https://img.shields.io/badge/single%20binary-CGO__ENABLED%3D0-informational)
![Runtimes](https://img.shields.io/badge/runtimes-Claude%20Code%20%C2%B7%20OpenCode-8A2BE2)

**witness is a local memory & self-improvement engine for Claude Code and OpenCode.** It captures
your coding sessions and distills how your patterns, habits, and knowledge **evolve over time** —
a person-centric growth archive with provenance, served over an MCP server + plain files, as a
single pure-Go binary. Think *second brain / AI memory* for how you think and grow, not project
memory for what your code did. *(File & PDF ingestion is on the way — see the roadmap — so the
same engine can distill how knowledge evolves across your notes and documents too.)*

> *"Aah, you were at my side, all along.*  
> *My true mentor...*  
> *My guiding moonlight..."*  
> — Ludwig, the Holy Blade

A Claude Code / OpenCode plugin that quietly keeps a **person-centric archive of how you grow
and change** as you work — your thinking, workstyle, habits, the cognitive traps you fall into
and climb out of, and how all of it shifts over time. Not a record of *what your code did*
(other tools do project memory) — a record of **who you are becoming**.

It is **coach-oriented, not clone-oriented**: the point is to let Claude understand you and reflect
you back to yourself, and to leave you a re-readable record of how you thought and grew. It is a
**pure tool** — it captures, structures, and serves the archive (via an MCP server and plain
files). Building a *coach* on top of it (proactive reflections, "you've done this three times…")
is left to other projects that read its output.

**Contents:** [How it works](#how-it-works) · [Lenses](#lenses) · [Example](#example-one-moment-end-to-end) · [Reading the archive](#reading-the-archive) · [Commands](#commands) · [Install](#install) · [Configuration](#configuration) · [Your data](#your-data-is-yours)

## How it works

Four layers — one ground-truth, three derived and **regenerable** from it:

| Layer | Kind | What it is |
|-------|------|------------|
| **raw (L0)** | ground truth | Every turn captured verbatim — from stable Claude Code hook fields (`UserPromptSubmit.prompt`, `Stop.last_assistant_message`) or OpenCode's local SQLite session DB (`message`/`part` text). Append-only, never LLM-touched. |
| **observations (L1)** | derived | A cheap per-session worker mines atomic, evidence-anchored observations about *you*, tagged by lens. Append-only. |
| **facets (L2)** | derived, bi-temporal | A periodic reviewer synthesizes observations into evolving *facets*, each keeping its **change history** (`valid_from`/`valid_to`) — so the archive answers "how did I change," not just "who am I now." Old values are never deleted. |
| **profile (L4)** | derived narrative | A short, human-readable markdown summary distilled from the facets, regenerated after each review: one per lens plus a cross-lens `unified` portrait. |

The archive is **collect-only / pull-only**: witness captures and distills everywhere, but never
injects anything into a session. Nothing is pushed — you (or an agent) read the profile on demand.
raw/observations/facets live in a single embedded SQLite database (`witness.db`); the profile is
plain markdown under `profile/`.

### Lenses

Every observation/facet carries a **lens** tag:

- **`default`** — global, runs on every session, cross-domain. This is the part no single-domain
  tracker can be: it sees that "diagnoses gaps precisely" fires in math *and* coding *and* career.
- **registered lenses** (e.g. `math`) — domain-specific lenses you **register once** and **enable
  globally**. `witness lens register math ./math/` adds the definition (a directory) to a central
  registry; `witness lens enable math` makes it run on every session (alongside `default`). Lenses
  are shared, not tied to any repo, so the same `math` lens covers all your math work.

#### Writing a lens

A lens is a **directory** of three files:

```
math/
  lens.json     settings: name, dimensions, optional per-lens models
  extract.md    per-session — mines observations (the whole file is the prompt)
  review.md     periodic — synthesizes observations into facets (the whole file is the prompt)
```

```json
// math/lens.json
{ "name": "math", "dimensions": ["speed", "independence", "proof_rigor", "abstraction", "confusion_tolerance"] }
```

```markdown
<!-- math/extract.md -->
You are observing one session through a MATH-LEARNING lens. Notice things about the
person as a mathematician — how they reason, get stuck, and climb out…
Return ONLY a JSON array. Each element:
[{ "dimension": "proof_rigor", "observation": "…", "evidence": "…", "poignancy": 6 }]
```

The one rule to remember: each prompt file is used **verbatim as the system prompt** and *replaces*
the built-in `default` prompts — it doesn't extend them — so each must be **self-contained,
including its output JSON schema** (the tool appends the transcript / observations as the user
message, but injects no schema for you).

A **complete, copy-paste-ready** lens lives at [`prompts/lens/example/`](prompts/lens/example) —
the fastest way to start is to copy the directory and rewrite the dimensions and prose for your
domain:

```sh
cp -R "$CLAUDE_PLUGIN_ROOT/prompts/lens/example" ./math   # edit the files, then:
witness lens register math ./math      # copies the definition into your store (a snapshot)
witness lens enable  math               # start running it on every session
```

`register` stores a **copy** — editing the original afterward has no effect until you re-register.
`enable` is the separate switch that makes it actually run.

**Per-lens models (optional).** By default every lens rides the global models (`witness config set
triage_model / distill_model`). A rare heavy lens can pin a stronger model just for itself —
without paying for it on every session — with `witness lens set math --extract-model <m>`
(and `--review-model <m>`); pass an empty value to clear it and ride the global again.

The source directory may live anywhere. As a recommended canonical location, witness keeps the
registered copy beside `config.toml` under `<witness-data-dir>/lenses/<name>/` (normally
`~/.local/share/witness/lenses/<name>/`, or `$WITNESS_HOME/lenses/<name>/`). You can edit that
registered copy directly, but this location is a convention rather than a restriction on the
directory passed to `lens register`.

## Example: one moment, end to end

Say a session contains this exchange (fictional):

> **you:** the migration keeps failing on prod but passes locally — I'll just run it by hand and move on
>
> **you:** …wait, what's actually *different* about prod? let me diff the two schemas before I touch anything

Here's what each layer makes of it.

**raw (L0)** — captured verbatim, nothing interpreted:

```
user  the migration keeps failing on prod but passes locally — I'll just run it by hand and move on
user  wait, what's actually different about prod? let me diff the two schemas before I touch anything
```

**observations (L1)** — the worker mines one atomic, evidence-anchored noticing:

```
[thinking] Caught the urge to hand-patch around a failure and redirected to isolating the
           prod/local difference before acting.
  evidence: "run it by hand and move on" → "what's different about prod? diff before I touch anything"
  poignancy: 6    lens: default
```

**facets (L2)** — after several such moments the reviewer synthesizes an evolving attribute, and
**keeps the history** (the whole point — it shows *change*, not just current state):

```
default · thinking · diagnoses_before_acting                        confidence 0.82
  2026-05 → now       Catches the reflex to work around a failure and isolates the
                      mechanism first; gates action on understanding the cause.
  2026-02 → 2026-05   Tended to apply the first workaround that unblocked the task.   (superseded)
```

**profile (L4)** — the narrative you actually read (`witness profile`):

> ## default
>
> You've been converging on a diagnose-first way of working. A few months ago the pattern was to
> reach for whatever unblocked the task; now you routinely catch that urge and turn to isolating the
> mechanism before you touch anything…

Nothing here is pushed into your sessions — you read it when you want it (`witness profile`), or an
agent pulls the relevant facet on demand.

## Reading the archive

Humans read the **narrative**; agents read the **structured** data. Over MCP:

- `get_profile(lens)` — the narrative profile (prose); omit `lens` for the unified portrait.
- `get_facets(lens)` — the current structured facets.
- `search_observations(query, lens)` — local vector search over observations.
- `record_observation(...)` — an in-session agent writes a decision-aware observation directly
  (passed through verbatim), capturing context a later reviewer would miss.
- `delete_observation(obs_id)` — prune a wrong observation.

## Commands

`witness <doctor | profile | facets | observations | review | lens | import | distill | cleanup | export | install | uninstall>` (capture,
the worker, and the MCP server are internal entry points invoked by Claude Code/OpenCode, not typed
by hand):

- `witness profile [lens]` — print the narrative profile (default: the unified portrait).
- `witness facets [lens]` — print current structured facets (CLI equivalent of MCP `get_facets`).
- `witness observations search <query> [--lens <lens>] [-k N]` — semantic search over observations.
- `witness observations record --session <id> --dimension <name> --observation <text>` — stage an active observation and kick the worker.
- `witness observations delete <obs_id>` — prune a wrong observation.
- `witness review` — force an L2 review and regenerate L4 profiles from existing observations.
- `witness lens register|enable|disable|list` — manage lenses.
- `witness import --agent opencode` — incrementally reconcile OpenCode's local session DB into L0
  and kick background distillation without waiting.
- `witness import --agent claude` — kick distillation for already-captured Claude Code hook data.
- `witness distill start|status|stop` — manage the background distillation worker. Manual starts
  accept `--since`/`--until` to select pending sessions by their latest raw timestamp; for example,
  `witness distill start --since 7d` distills sessions updated in the last seven days. Bounds also
  accept RFC3339 timestamps or UTC dates (`YYYY-MM-DD`) and do not discard sessions outside the
  selected range.
- `witness cleanup` — interactively reclaim old raw transcripts (keeps observations + profile).
- `witness export <path>` — write a consistent single-file snapshot of the archive (safe to back up / cloud-sync).
- `witness doctor` — health check (verifies the embedder runs and EN/ZH retrieval works).

## Single binary, no runtime

The whole thing is **one self-contained Go binary** — no Python, no external services, no vector
DB, no cloud key. Local multilingual (English **and** Chinese) embeddings run pure-Go via GoMLX
(`CGO_ENABLED=0`, verified: matches ONNX Runtime exactly). Distillation defaults to your existing
Claude Code auth via `claude -p`; set `runner = opencode` to use a private `opencode serve` runner instead.

## Install

```sh
./install.sh claude    # Claude Code: build, fetch model (~448MB once), wire hooks + MCP
./install.sh opencode  # OpenCode: build, fetch model, install local plugin + MCP
```

That's the whole thing — idempotent, safe to re-run after a `git pull`. The target
is required: install always binds the matching distillation runtime into
`config.toml` (`runner = claude` or `runner = opencode`). It also offers to add a
`witness` command to your PATH (for `witness profile`, `doctor`, `lens`, `import`,
`distill`, `cleanup`). Equivalent `make` targets exist (`make install`,
`make install-opencode`, `make build`, `make doctor`, `make uninstall`,
`make uninstall-opencode`, `make clean`). To remove it: `make uninstall` or
`make uninstall-opencode` (strips integration wiring; your data is untouched).

### Windows

Windows uses a self-contained zip instead of the shell installer (there is no
guaranteed shell to run the hook shim). Download `witness-windows-amd64.zip`
(Intel/AMD) or `witness-windows-arm64.zip` from the releases page — each unpacks
to a `witness\` folder holding `witness.exe` and the embedding model. Then, from
inside that folder in PowerShell:

```powershell
.\witness.exe install claude
```

This copies the bundle into `%LOCALAPPDATA%\witness`, adds it to your user PATH,
and wires Claude Code with **exec-form hooks** pointing at `witness.exe` (no shell,
no Git Bash needed). The zip carries the prompt templates and the ~448MB model
alongside the exe; the binary resolves both relative to itself. Uninstall strips
the hooks + MCP (`witness.exe uninstall claude`); the copied files and PATH entry
are left in place for now.

### OpenCode support

OpenCode support has two pieces:

- A plugin reconciles OpenCode's SQLite DB on startup and when a session goes idle, then asks the
  laptop-friendly auto-start gate to distill when allowed. From-source installs write a
  local plugin to `~/.config/opencode/plugins/witness.js`; published installs can use the npm plugin
  `@witness-ai/opencode`.
- An OpenCode MCP entry named `witness` launches the same MCP server as Claude Code, exposing
  `get_profile`, `get_facets`, `search_observations`, `record_observation`, and
  `delete_observation`.

The npm package ships the OpenCode plugin, a `witness` CLI shim, prebuilt witness binaries, and prompts.
The config-only path is the default: add the plugin to `~/.config/opencode/opencode.json`, and OpenCode
installs it automatically with Bun on startup. If `mcp.witness` is absent, the plugin auto-registers it
for you.

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
not support `witness install` / `witness uninstall`; those commands are for source-checkout installs.
Custom model mirrors must provide `WITNESS_MODEL_SHA256` and `WITNESS_TOKENIZER_SHA256` alongside
`WITNESS_MODEL_BASE_URL`.

After the first download, verify the model, OpenCode runner, archive, and queue:

```sh
npm exec --yes --package=@witness-ai/opencode@beta -- witness doctor
npm exec --yes --package=@witness-ai/opencode@beta -- witness distill status
```

The npm package lives in [`npm/opencode`](npm/opencode). Stage prebuilt binaries and prompts before publishing:

```sh
make npm-opencode-package
npm publish ./npm/platform/darwin-arm64 --access public --tag beta
npm publish ./npm/platform/linux-x64 --access public --tag beta
(cd npm/opencode && npm publish --access public --ignore-scripts --tag beta)
```

Configure npm Trusted Publishing separately for the main package and both platform packages before using
the release workflow. The npm package page renders [`npm/opencode/README.md`](npm/opencode/README.md);
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
witness distill status             # watch non-blocking distillation progress
witness review                     # forces L2 facets + L4 markdown profiles
witness profile opencode           # per-lens L4 report
witness profile                    # unified L4 report
```

## Configuration

`~/.local/share/witness/config.toml` (all optional; sensible defaults):

```toml
runner           = "claude"            # "claude" (default) or "opencode"
triage_model     = "claude-haiku-4-5"   # cheap per-session mining ("" = claude -p default)
distill_model    = "claude-opus-4-8"    # the reviewer ("" = claude -p default)
review_every     = 5                    # run the reviewer every N distilled sessions...
review_poignancy = 30                   # ...or sooner once accumulated salience crosses this (0 = off)
auto_distill = true                     # hooks/plugins may start model work automatically
auto_distill_interval_minutes = 10      # minimum gap between automatic worker starts
auto_distill_session_budget = 0         # sessions per automatic run (0 = drain current queue)
```

Set `auto_distill = false` for capture-only mode on battery-constrained machines, then run
`witness distill start` manually when plugged in. Automatic workers are short-lived: they load the
embed model only while draining queued sessions, then exit.

When `runner = opencode`, `triage_model` and `distill_model` should use OpenCode model names such
as `openai/gpt-5.5`; empty values use your OpenCode defaults. Non-empty OpenCode model names are
validated against `opencode models <provider>` before distillation, and `witness doctor` reports the
same check as `opencode models: OK` or an explicit invalid-model error.

Enabled lenses are managed for you (`witness lens enable/disable <name>`) and appear as simple
lines, each naming a registered lens that runs on every session:

```toml
lens = math
```

There is no automatic retention knob: raw transcripts are kept until you deliberately reclaim
them with `witness cleanup` (which never touches your observations or profile).

## Your data is yours

Everything lives under `~/.local/share/witness/` (override with `WITNESS_HOME`; installs
predating the rename keep using `~/.local/share/claude-witness/`, adopted automatically), is `0700`
(the DB and profile files `0600`), and never leaves your machine. The repo ships the framework,
schema, and prompts — **never anyone's archive.**

**Backup / sync.** To back the archive up or sync it (iCloud/Dropbox/Drive), use `witness export
<path>` — it writes a single consistent `.db` snapshot you can point a syncer at. Do **not** sync the
live data directory directly: the database runs in WAL mode (`.db` + `-wal` + `-shm`), and a syncer
racing those files can corrupt it. Wire it up yourself, e.g. a cron/launchd job:

```sh
witness export ~/Dropbox/witness-backup.db --force   # consistent snapshot, safe to sync
```

To restore, stop witness and copy a snapshot into your data dir as `witness.db` (or set `WITNESS_HOME`
to its folder), then run `witness review` — the snapshot holds the source of truth (raw turns,
observations, facets); the narrative profile is regenerated from it.

## License

MIT — see [LICENSE](LICENSE).
