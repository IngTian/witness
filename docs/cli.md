# Command and configuration reference

Every command and every config key. For what witness is and how to install it, see the [README](../README.md).

## Commands

The visible front door, exactly as `witness --help` groups it:

| group | commands |
|---|---|
| Read your archive | `status` · `profile` · `view` · `facets` · `observations` · `ingest` |
| Lenses | `lens` |
| Configure | `config` |
| Setup | `doctor` · `install` · `model` · `wire` · `unwire` |
| Maintenance | `cleanup` · `export` · `merge` · `compact` · `worker` |

Capture, the automatic worker runs, and the MCP server are **internal** entry points invoked by
Claude Code, OpenCode and Codex, not typed by hand. `witness worker …` is the operator side of
that worker, and `witness import` is hidden plugin and hook plumbing. Three jobs happen only when
you run them: distilling while `auto_distill = false` (`witness worker run`), the long-arc pass
(`witness worker review --full`), and importing Codex history from before you wired it
(`witness import --agent codex`).

- `witness profile [lens]` — print the narrative profile (default: the unified portrait).
- `witness view [--no-open] [--out <path>]` — write the archive as one self-contained HTML page
  (`view.html` in the data dir, or `--out`) and open it in your browser. A snapshot: run it again to
  refresh. It changes nothing and never calls a model, so an out-of-date profile is marked on the
  page rather than rebuilt (`witness profile` rebuilds it). The page includes the observations
  behind each facet; keep it as private as the archive. `export --all` leaves it out, since it is
  rebuilt from what the backup holds.
- `witness facets [lens]` — print the active structured facets (CLI equivalent of MCP `get_facets`).
  `--all` also lists archived facets, saying where each one went; `--expand
  <dimension/key>` unfolds one facet into everything that was merged into it.
- `witness compact [--lens <name>] [--yes]` — consolidate the profile: merge facets that describe
  one trait, retire ones that were never traits, and give each surviving facet a short `claim`
  (the summary the review call reads instead of the full prose). Claims don't wait for compact:
  every review fills in a lens's missing claims before it folds, up to about 200 per run. Compaction
  itself stays manual, and `status` and `doctor` suggest it once a lens has grown a lot. **It prints the plan, then asks
  `[y/N]`**; only `y` applies it, and the plan applied is exactly the one shown. `--yes` skips the
  question for scripts (`--apply`, its old name, still works). Nothing is deleted: a compacted facet is archived with its
  full version history and a link to the facet that absorbed it, so `--expand` can always unfold
  it. Re-runnable; each pass consolidates what it can see.
- `witness observations search <query> [--lens <lens>] [-k N]` — semantic search over observations.
- `witness observations delete <obs_id>` — prune a wrong observation. It is removed now; facets that rested only on it
  are retired at the next review, and nothing is re-read.
- `witness worker candidates <lens>` — dry-run the long-arc clustering for one lens: prints the
  candidate arcs it would verify, with no model call and nothing written.
- `witness worker review [--full]` — force an L2 review and regenerate L4 profiles from existing
  observations. Review normally runs on its own schedule (`review_every` / `review_poignancy`),
  so reaching for this by hand is the exception. `--full` also runs the emergent long-arc pass,
  which nothing runs automatically.
- `witness lens register|deregister|enable|disable|list` — manage lenses.
- `witness lens show <name>` — print one lens's settings and a prompt excerpt (`--prompts` prints
  both prompts in full).
- `witness lens try <file|lens-name>` — preview a lens's extract prompt on real sessions without
  writing anything; `--review` also previews the facets its review prompt would produce.
- `witness lens backfill <name> [--fresh]` — re-mine one lens over the whole history and refresh its
  facets; `--fresh` first drops the lens's observations + facets (for a changed prompt).
- `witness lens load-default` — re-seed / restore the built-in "default" person-growth lens (it is
  auto-seeded once on first use and fully deletable, so this is how you bring it back).
- `witness import --agent opencode` — incrementally reconcile OpenCode's local session DB into L0
  and kick background distillation without waiting.
- `witness import --agent codex` — import Codex sessions; see [Codex capture](codex-capture.md).
- `witness status` — what has been captured, whether the worker is running, and how fresh the
  distilled data is (`--json` for scripts).
- `witness worker run` / `witness worker stop` — the operator escape hatch for the background
  distillation worker (normally it runs off editor hooks). `run` drains in
  the foreground, `--detach` backgrounds it. With `--detach` it accepts `--since`/`--until` to
  select pending sessions by their latest raw timestamp — for example
  `witness worker run --detach --since 7d` distills sessions updated in the last seven days. Bounds also accept RFC3339 timestamps or UTC dates
  (`YYYY-MM-DD`) and do not discard sessions outside the selected range.
- `witness cleanup` — interactively reclaim old raw transcripts (keeps observations + profile).
- `witness export <path>` — write a consistent single-file snapshot of the **database** (safe to back
  up / cloud-sync). Add `--all` and `<path>` becomes a **directory** holding the complete archive:
  that snapshot plus `config.toml`, `lenses/`, `summarize/` (your own portrait prompts, if any)
  and `profile/`. Use `--all` for backups — see
  [Your data is yours](../README.md#your-data-is-yours).
- `witness merge <dir>` — merge another archive into this one, for a machine that already has its own
  (copying a backup in would replace it). `<dir>` is an `export --all` directory, or a single `.db`
  from a plain `export`. It shows what would arrive, then asks `[y/N]`; on `y` it backs this archive up,
  merges, and prints how to undo. Declining writes nothing and exits non-zero; `--yes` skips the
  question for scripts (it still backs up). Nothing is replaced and the model is never called: sessions, observations and facets
  are unioned with their full histories, and a session both machines mined is not imported twice. Where
  both archives minted the same facet key, the copy with newer evidence keeps it and the other is
  archived beside it; duplicates under different keys are left for `witness compact`. A one-time
  consolidation, not a sync — merged back and forth, two machines grow an archived copy of every facet
  both changed, every round.
- `witness install [--path <dir>]` — provision a witness archive at the specified path (or the
  default data root) without wiring any editor. Creates the directory structure and database schema.
  An archive chosen with `--path` or `WITNESS_HOME` starts with no lenses, for a library or data
  source; the default data root gets the built-in `default` lens like any first open
  (`WITNESS_NO_DEFAULT_LENS=1` keeps it blank). A personal machine does not need it: `install.sh`
  runs `witness wire`, which opens the archive itself.
- `witness wire <claude|opencode|codex>` — wire the editor integration (hooks + MCP for Claude Code;
  plugin + MCP for OpenCode; hooks for Codex). Source-checkout command; `install.sh` calls this for
  you. Re-running it is safe: it replaces its own entries and preserves any hooks you wrote yourself.
  Two notes specific to Codex: it runs no model, so wiring it leaves your `runner` alone, and its
  hooks stay inert until you approve them with `/hooks` inside Codex — wiring cannot grant that trust.
- `witness unwire <claude|opencode|codex>` — remove the editor integration wiring. Your archive is untouched.
- `witness ingest [--file <path>]` — accept structured records (notes, logs, market data) as NDJSON
  and distill them into the archive. Reads from stdin or a file. See the record contract in
  [`prompts/SCHEMA.md`](../prompts/SCHEMA.md#records-in-the-ingest-contract).
- `witness doctor` — health check (verifies the embedder runs and EN/ZH retrieval works).
  In `doctor --json`'s `config` block, `triage_model` and `distill_model` are the raw config.toml
  fields; `runner`, `mine_model` and `review_model` are what actually runs, each model with a
  `_from` key saying where it came from.
- `witness model fetch` — download the embedding model (multilingual-e5-small, ~470MB) once, pinned
  to one revision and checked by SHA-256. `install.sh` and `make fetch-model` run it; re-running
  checks the files instead of downloading them again. `WITNESS_ASSETS` picks the directory, and
  `WITNESS_MODEL_BASE_URL` a mirror (it needs `WITNESS_MODEL_SHA256` and
  `WITNESS_TOKENIZER_SHA256`). From npm, `npx @witness-ai/opencode mcp` downloads the model itself
  in the background if it is missing (`WITNESS_SKIP_MODEL_DOWNLOAD=1` turns that off).
- `witness config get|set|unset|list [--lens <name>]` — read or change the runner and models, for
  the default scope or one lens; `witness config path` prints where `config.toml` lives.

## Configuration

`~/.local/share/witness/config.toml` (`witness config path` prints yours; all optional; sensible defaults):

```toml
runner           = "claude"   # "claude" (default) or "opencode"
triage_model     = ""         # MINING (L0 -> L1), once per session; "" = the runner's default
distill_model    = ""         # REVIEW (L1 -> L2/L4), batched; "" falls back to triage_model
review_every     = 5          # run the reviewer every N distilled sessions...
review_poignancy = 30         # ...or sooner once accumulated salience crosses this (0 = off)
auto_distill     = true       # hooks/plugins may start model work automatically
mine_concurrency = 4          # parallel per-session miners (<= 0 restores the default)
chunk_max_chars  = 0          # 0 = mine each session WHOLE (best quality); set a positive
                              #     value only if a giant session times out
review_max_chars = 16000      # safety ceiling: max observation chars folded in ONE review
                              #     call, so a big backlog cannot ride the runner timeout
```

Model ids are passed through to whichever runner is bound, so use that runtime's spelling —
`claude-haiku-4-5` or `claude-sonnet-5` for `runner = "claude"`, `openai/gpt-5.5`-style ids for
`runner = "opencode"`. Leaving both empty is the safe default and what `witness wire` writes;
note that an empty `triage_model` under the claude runner inherits your *ambient interactive*
model, which may be heavier than you want for per-session mining, so pinning a light one is
usually worth it.

`chunk_max_chars` and `review_max_chars` are **safety ceilings, not quality dials.** Mining a
session whole is the best-quality path — splitting it loses insight that spans the session, like a
decision made early and confirmed late — so raise `chunk_max_chars` off 0 only if a giant session
actually times out. `review_max_chars` exists so a large review backlog is folded in windows
rather than one call that rides the 10-minute runner timeout and freezes the profile.

Set `auto_distill = false` for capture-only mode on battery-constrained machines, then run
`witness worker run --detach` manually when plugged in. Automatic workers are short-lived: they load the
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

---

[← back to the README](../README.md)
