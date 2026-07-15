# witness schema

The data model in one page. Four layers: one ground-truth, three derived and regenerable.
Everything lives in a single embedded SQLite database, `witness.db`, under the data root
(`$WITNESS_HOME`, else `$XDG_DATA_HOME/witness`, else `~/.local/share/witness`).
The one exception is the L4 profile, written as plain markdown files so you can read them directly.

Layer vocabulary: **raw → observations → facets → profile** (`L0/L1/L2` are shorthand; L3 is
intentionally unused — the profile sits directly on the facets).

## raw — ground-truth transcript (L0)

Table `raw`, append-only. One row per turn-half, captured verbatim from stable hook fields
(`UserPromptSubmit.prompt` / `Stop.last_assistant_message`). Never LLM-touched. Columns:
`id, session, seq, ts, role, effort, text`. Reclaim old rows with `witness cleanup` (there is no
automatic retention — pruning is an explicit, confirmed action).

## observations — derived, append-only (L1)

Table `observations`. Atomic, evidence-anchored noticings, tagged by lens. Written ONLY by the
worker (which combines active + mined and dedups on `obs_id`, a content hash). Each carries a
384-d embedding (BLOB) for recall and dedup. Columns: `obs_id, ts, session, lens, dimension,
observation, evidence, poignancy, source, embedding`. `source` is `mined` or `active`.

## facets — derived, bi-temporal (L2)

Tables `facets` + `facet_versions`. Evolving named attributes within a lens+dimension. Written
ONLY by the reviewer. A facet's ordered `facet_versions` rows ARE its change history:
`value, valid_from, valid_to, recorded_at, because_of (JSON obs-id array), confidence`.
`valid_to == ''` means current. Old versions are kept, never deleted — they are the trajectory.

### The invalidation rule (why this stays honest)

`valid_to` is set ONLY on **positive evidence** the window ended:
1. a **sustained contradicting pattern** (a real change arc), or
2. **recency expiry** for time-bound `state` facets.

NEVER on mere absence — not seeing a facet lately only decays its confidence; it does not close
it. This prevents the reviewer from fabricating "you stopped doing X" arcs out of silence.

## profile — narrative summary (L4)

Plain markdown under `<root>/profile/`: one file per lens (`<lens>.md`) plus a cross-lens
`unified.md`. A short prose rendering distilled from the facets by the summarizer, regenerated in
the background after each review. Read it with `witness profile [lens]` or the `get_profile` MCP
tool. (`unified` is reserved and cannot be a lens name.)

## Lenses

Every observation/facet carries a `lens` tag.

- **`default`** — global, runs on every session, cross-domain. This is the thing no single-domain
  tracker can be.
- **registered lenses** (e.g. `math`) — registered centrally (`witness lens register <name> <file>`)
  and enabled globally (`witness lens enable <name>`); an enabled lens runs on every session.
  Definitions live in the central registry (`<root>/lenses/<name>/lens.md`), shared across all
  sessions — never read from a repo.

A lens file's header carries `# name:` and `# dimensions:`. See `prompts/lens/example.md`. Mining
uses one global model for every lens — there is no per-lens model today (a lens that needs a
stronger model just means "set the global runner to a capable one" via `witness config set
triage_model <model>`; per-lens model tuning is tracked separately).

The profile is **collect-only / pull-only**: witness captures and distills everywhere, but never
injects into a session. Agents read it on demand via the MCP tools (`get_facets`, `get_profile`,
`search_observations`); humans read it via `witness profile`.

One moment can produce several observations — one per lens that finds it salient — sharing a raw
anchor but framed for each lens.
