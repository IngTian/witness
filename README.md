# claude-witness — Let Claude Code witness your growth.

> *"Aah, you were at my side, all along.*  
> *My true mentor...*  
> *My guiding moonlight..."*  
> — Ludwig, the Holy Blade

A Claude Code plugin that quietly keeps a **person-centric archive of how you grow and change**
as you work — your thinking, workstyle, habits, the cognitive traps you fall into and climb out
of, and how all of it shifts over time. Not a record of *what your code did* (other tools do
project memory) — a record of **who you are becoming**.

It is **coach-oriented, not clone-oriented**: the point is to let Claude understand you and reflect
you back to yourself, and to leave you a re-readable record of how you thought and grew. It is a
**pure tool** — it captures, structures, and serves the archive (via an MCP server and plain
files). Building a *coach* on top of it (proactive reflections, "you've done this three times…")
is left to other projects that read its output.

## How it works

Four layers — one ground-truth, three derived and **regenerable** from it:

- **raw (L0) — ground truth.** Every turn is captured verbatim from stable hook fields
  (`UserPromptSubmit.prompt`, `Stop.last_assistant_message`) — no fragile transcript parsing.
  Append-only, never LLM-touched.
- **observations (L1) — derived.** A cheap per-session worker mines atomic, evidence-anchored
  observations about *you*, tagged by lens. Append-only.
- **facets (L2) — derived, bi-temporal.** A periodic reviewer synthesizes observations into
  evolving *facets*, each keeping its **change history** (`valid_from`/`valid_to`) — so the archive
  answers "how did I change," not just "who am I now." Old values are never deleted.
- **profile (L4) — derived narrative.** A short, human-readable markdown summary distilled from the
  facets, regenerated after each review: one per lens plus a cross-lens `unified` portrait.

The archive is **collect-only / pull-only**: witness captures and distills everywhere, but never
injects anything into a session. Nothing is pushed — you (or an agent) read the profile on demand.
raw/observations/facets live in a single embedded SQLite database (`witness.db`); the profile is
plain markdown under `profile/`.

### Lenses

Every observation/facet carries a **lens** tag:

- **`default`** — global, runs on every session, cross-domain. This is the part no single-domain
  tracker can be: it sees that "diagnoses gaps precisely" fires in math *and* coding *and* career.
- **registered lenses** (e.g. `math`) — domain-specific lenses you **register once** and **enable
  globally**. `witness lens register math ./math-lens.md` adds the definition to a central registry;
  `witness lens enable math` makes it run on every session (alongside `default`). Lenses are shared,
  not tied to any repo, so the same `math` lens covers all your math work.

## Reading the archive

Humans read the **narrative**; agents read the **structured** data. Over MCP:

- `get_profile(lens)` — the narrative profile (prose); omit `lens` for the unified portrait.
- `get_facets(lens)` — the current structured facets.
- `search_observations(query, lens)` — local vector search over observations.
- `record_observation(...)` — an in-session agent writes a decision-aware observation directly
  (passed through verbatim), capturing context a later reviewer would miss.
- `delete_observation(obs_id)` — prune a wrong observation.

## Commands

`witness <doctor | profile | lens | cleanup | install | uninstall>` (capture, the worker, and the
MCP server are internal entry points invoked by Claude Code, not typed by hand):

- `witness profile [lens]` — print the narrative profile (default: the unified portrait).
- `witness lens register|enable|disable|list` — manage lenses.
- `witness cleanup` — interactively reclaim old raw transcripts (keeps observations + profile).
- `witness doctor` — health check (verifies the embedder runs and EN/ZH retrieval works).

## Single binary, no runtime

The whole thing is **one self-contained Go binary** — no Python, no external services, no vector
DB, no cloud key. Local multilingual (English **and** Chinese) embeddings run pure-Go via GoMLX
(`CGO_ENABLED=0`, verified: matches ONNX Runtime exactly). Distillation reuses your existing
Claude Code auth via `claude -p`.

## Install

```sh
./install.sh        # builds the binary, fetches the model (~448MB, once), wires hooks + MCP
```

That's the whole thing — idempotent, safe to re-run after a `git pull`. Equivalent `make`
targets exist (`make install`, `make build`, `make doctor`, `make uninstall`, `make clean`).
To remove it: `make uninstall` (strips the hooks + MCP server; your data is untouched).

## Configuration

`~/.local/share/claude-witness/config.toml` (all optional; sensible defaults):

```toml
triage_model     = "claude-haiku-4-5"   # cheap per-session mining ("" = claude -p default)
distill_model    = "claude-opus-4-8"    # the reviewer ("" = claude -p default)
review_every     = 5                    # run the reviewer every N distilled sessions...
review_poignancy = 30                   # ...or sooner once accumulated salience crosses this (0 = off)
```

Enabled lenses are managed for you (`witness lens enable/disable <name>`) and appear as simple
lines, each naming a registered lens that runs on every session:

```toml
lens = math
```

There is no automatic retention knob: raw transcripts are kept until you deliberately reclaim
them with `witness cleanup` (which never touches your observations or profile).

## Your data is yours

Everything lives under `~/.local/share/claude-witness/` (override with `WITNESS_HOME`), is `0700`
(the DB and profile files `0600`), and never leaves your machine. The repo ships the framework,
schema, and prompts — **never anyone's archive.**

## Status

🌱 v0. Capture, single-binary embedder, MCP server, and the distillation pipeline are built and the
plumbing is verified end-to-end. The distillation *prompts* (the quality-critical part) are first
drafts meant to be tuned against real logs.

## License

MIT — see [LICENSE](LICENSE).
