# witness schema

The data model in one page. Four layers: one ground-truth, three derived and regenerable.
Everything lives in a single embedded SQLite database, `witness.db`, under the data root
(`$WITNESS_HOME`, else `$XDG_DATA_HOME/witness`, else `~/.local/share/witness`).
The one exception is the L4 profile, written as plain markdown files so you can read them directly.
Columns are defined once, in the `schemaV1` block of [`internal/store/db.go`](../internal/store/db.go);
this page says what the tables mean.

Layer vocabulary: **raw → observations → facets → profile** (`L0/L1/L2` are shorthand; L3 is
intentionally unused — the profile sits directly on the facets).

## raw — ground-truth transcript (L0)

Table `raw`, append-only. One row per captured turn-half or ingested record, verbatim; never
LLM-touched. Reclaim old rows with `witness cleanup` (there is no automatic retention — pruning is
an explicit, confirmed action).

## observations — derived, append-only (L1)

Table `observations`. Atomic, evidence-anchored noticings, tagged by lens. Written by the worker
(`source` is `mined`, or `active` for one an agent recorded mid-session) and by `witness merge`.
`obs_id` hashes session + lens + text, so an identical re-mine is ignored; near-duplicates are
kept. Each row carries a 384-d embedding for semantic search and the long-arc clustering.

## facets — derived, bi-temporal (L2)

Tables `facets` + `facet_versions`. Evolving named attributes within a lens+dimension, written by
the review fold, the long-arc pass, compaction, the claim pass and `witness merge`. A facet's
ordered `facet_versions` rows ARE its change history; the version with `valid_to == ''` is current.
Old versions are kept, never deleted — they are the trajectory. A facet is in the live profile only
while `archived_at == ''`: compaction and `merge` archive a facet into another (`merged_into`)
rather than delete it. How confidence moves and decays is in
[how it works](../docs/how-it-works.md#confidence-and-why-it-moves-on-its-own).

### The invalidation rule (why this stays honest)

`valid_to` is set ONLY on **positive evidence** the window ended:
1. a **sustained contradicting pattern** (a real change arc), or
2. **recency expiry** for time-bound `state` facets.

NEVER on mere absence — not seeing a facet lately only decays its confidence; it does not close
it. This prevents the reviewer from fabricating "you stopped doing X" arcs out of silence.

## profile — narrative summary (L4)

Plain markdown under `<root>/profile/`: one file per lens (`<lens>.md`) plus a cross-lens
`unified.md`. A short prose rendering distilled from the live facets by the summarizer, regenerated
after each review and on read when the facets changed. Read it with `witness profile [lens]` or the
`get_profile` MCP tool. (`unified` is reserved and cannot be a lens name.)

## Lenses

Every observation/facet carries a `lens` tag. What the `default` and registered lenses are and how
they run is in [how it works](../docs/how-it-works.md#lenses); definitions live in the central
registry (`<root>/lenses/<name>/`), never in a repo.

A lens is a **directory** of `lens.json` (settings: `name`, `dimensions`, optional per-lens models)
plus `extract.md` and `review.md` (each whole file is the prompt for its pass). See
`prompts/lens/example/`. Mining uses the default model by default, but a lens may pin its own with
`witness config set --lens <name> mine_model|review_model <model>` (or `extract_model` /
`review_model` in its `lens.json`, then re-register: the store keeps a snapshot), verified with
`witness lens show <name>`,
so a rare heavy lens can run a stronger model without paying for it on every session.

One moment can produce several observations — one per lens that finds it salient — sharing a raw
anchor but framed for each lens.

## Records-in: the `ingest` contract

witness can accept records from external sources (notes, market data, timestamped logs) via the
`witness ingest` command. This is a **stable, versioned public interface** for bringing structured
text into the distillation engine as a first-class L0 source alongside captured sessions.

### Format

NDJSON (newline-delimited JSON): one JSON object per line, delivered via stdin or a file path.

```sh
witness ingest < records.ndjson
witness ingest --file records.ndjson
```

### Fields

Each record is a JSON object with these fields:

- **`text`** (REQUIRED, non-empty string) — the content to distill. This is what the extract lens sees.
- **`id`** (optional string) — the caller-defined deduplication identity. Records sharing the same
  `id` are considered versions of one logical entry. **Recommended** — supply an id when you have a
  natural stable identifier (a filename, a URL, a database primary key).
- **`session`** (optional string) — a caller-defined grouping key. Records sharing a `session` value
  are mined together as one chunk (one distillation unit), so the lens sees them in context. Omit
  `session` (or pass different values) to mine records independently. All ingested sessions are
  namespaced with the `file:` prefix in L0.
- **`ts`** (optional string) — the content timestamp in RFC3339 format (`YYYY-MM-DDTHH:MM:SSZ`) or
  UTC date (`YYYY-MM-DD`). Defaults to ingest-time if absent. This becomes the L0 row's `ts` and
  propagates to observations. Used for temporal facets (valid_from) and profile narrative.
- **`role`** (optional string) — defaults to `"document"`. May be any string; this becomes the L0
  `role` field. Reserved for future use (e.g., distinguishing user notes from assistant summaries).

Unknown fields are ignored (forward compatibility).

### Deduplication semantics

witness dedups records on the caller-provided `id`:

- **Same id + unchanged text** → skip (already ingested).
- **Same id + changed text** → update the existing L0 row with the new text and `ts`; observations
  are re-mined on next distillation (the lens sees the new version).
- **New id** (or no prior record with this id) → append as a new L0 row.
- **No id provided** → fall back to content-hash dedup (same hash → skip; new hash → append). Less
  precise — changing a single character creates a new row rather than updating the logical entry.

Re-ingesting the same record set is idempotent (no duplicate rows, no redundant observations).

### Grouping semantics

Records with the same `session` value mine as one batch — the lens reads the whole group in one
pass, so cross-record patterns are visible (e.g., a recurring theme across multiple notes). Omit
`session` (or use distinct values) to mine each record independently; witness derives a session id
from the record `id` (or content hash) so each record becomes its own session. All ingested sessions
are namespaced with `file:` in the L0 `session` field, so they never collide with captured agent
sessions.

### Example

```jsonl
{"id": "note-2026-07-01", "session": "weekly-retros", "ts": "2026-07-01", "text": "Caught myself rushing to a fix before isolating the cause. Stopped, wrote repro steps, found it in two minutes."}
{"id": "note-2026-07-08", "session": "weekly-retros", "ts": "2026-07-08", "text": "Same trap again — saw a failing test, went straight to the code. Forced myself to read the failure message first; saved an hour."}
```

After ingest, the worker (`witness worker run` to start it now) mines these as one session (they
share `session`), and the lens sees both notes together — enough context to notice a pattern.
