# How witness works

The layers, the lens system, and what each stage produces. If you just want to install it and read your profile, the [README](../README.md) is enough.

## How it works

Four layers — one ground-truth, three derived and **regenerable** from it:

| Layer | Kind | What it is |
|-------|------|------------|
| **raw (L0)** | ground truth | Every turn captured verbatim — from stable Claude Code hook fields (`UserPromptSubmit.prompt`, `Stop.last_assistant_message`) or OpenCode's local SQLite session DB (`message`/`part` text). Append-only, never LLM-touched. |
| **observations (L1)** | derived | A cheap per-session worker mines atomic, evidence-anchored observations about *you*, tagged by lens. Append-only. |
| **facets (L2)** | derived, bi-temporal | A periodic reviewer synthesizes observations into evolving *facets*, each keeping its **change history** (`valid_from`/`valid_to`) — so the archive answers "how did I change," not just "who am I now." Old values are never deleted. |
| **profile (L4)** | derived narrative | A short, human-readable markdown summary distilled from the facets — one per lens plus a cross-lens `unified` portrait. Generated **on read**: `witness profile` (or MCP `get_profile`) rebuilds it only if the facets changed, so an unread profile costs nothing. A cached read is instant; a rebuild takes ~13s. |

The archive is **collect-only / pull-only**: witness captures and distills everywhere, but never
injects anything into a session. Nothing is pushed — you (or an agent) read the profile on demand.
raw/observations/facets live in a single embedded SQLite database (`witness.db`); the profile is
plain markdown under `profile/`.

### Confidence, and why it moves on its own

Every facet carries a confidence between 0 and 1. It is not a score of you — it is how sure the
archive is that the claim currently holds.

It **rises** when new evidence reaffirms a facet and **falls** when the reviewer, having looked,
is less sure. Either way a single review moves it by at most 0.1, so confidence means "accumulated
across reviews" rather than "whatever the last window felt". A facet needs several reviews to climb
to a high number, which is the point: the number should track repeated independent evidence.

It also **decays when nothing cites the facet.** The clock is observations elapsed, not wall-clock
time — decay has to mean "evidence was possible and did not arrive", so a fortnight away from the
keyboard costs you nothing. The half-life is 3,000 observations: a facet last confirmed at 0.9
reads around 0.64 after a typical gap between citations, and approaches the floor after roughly
10,000 uncited observations.

**The floor is 0.1, and it never goes below.** Absence of evidence is not evidence of absence: a
quiet trait is still a trait, so decay marks a facet uncertain and never retires it. One citation
restores it.

So a confidence you saw last month may read lower today with no new sessions. That is working
correctly. If you want to know why a facet is where it is, `witness facets <lens>` shows the
current value and `witness facets <lens> --expand <dimension/key>` unfolds everything that was
merged into it.

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

Two **complete, copy-paste-ready** lenses ship as starting points. Copy the directory and rewrite the
dimensions and prose for your domain:

- [`prompts/lens/example/`](../prompts/lens/example) — a **person** lens (math learning), for tracking
  someone as they work.
- [`prompts/lens/corpus-example/`](../prompts/lens/corpus-example) — a **non-person** lens (market
  regime), for tracking a subject that is not you. Start here for research notes, a knowledge base,
  incident reports, or any document stream; its README explains what has to change when the subject
  stops being a person.

```sh
cp -R "$CLAUDE_PLUGIN_ROOT/prompts/lens/example" ./math   # edit the files, then:
witness lens register math ./math      # copies the definition into your store (a snapshot)
witness lens enable  math               # start running it on every session
```

`register` stores a **copy** — editing the original afterward has no effect until you re-register.
`enable` is the separate switch that makes it actually run.

**Per-lens models (optional).** By default every lens rides the default models (`witness config set
triage_model / distill_model`). A rare heavy lens can pin a stronger model just for itself —
without paying for it on every session — by adding `extract_model` / `review_model` to its
`lens.json`:

```json
{ "name": "math", "dimensions": ["proof_rigor"], "extract_model": "claude-sonnet-5" }
```

Omit a field (or leave it empty) to ride the default. Since `register` stores a snapshot, edit the
registered copy under `<witness-data-dir>/lenses/<name>/lens.json` — or edit the source and
re-register. Verify what a lens resolved to with `witness lens show <name>`.

The source directory may live anywhere. As a recommended canonical location, witness keeps the
registered copy beside `config.toml` under `<witness-data-dir>/lenses/<name>/` (normally
`~/.local/share/witness/lenses/<name>/`, or `$WITNESS_HOME/lenses/<name>/`). You can edit that
registered copy directly, but this location is a convention rather than a restriction on the
directory passed to `lens register`.

### Writing your own summary prompt

The shipped `unified` prompt writes a **personal growth portrait**. If your archive is something else
— market records, research notes, a project log — drop your own prompt in and it wins:

```
<data-root>/summarize/unified.md    your prompt (overrides the built-in one)
<data-root>/summarize/lens.md       same, for the per-lens summaries
```

`witness doctor` prints the data root. The file is the whole interface: no command to run, no
registration step. Delete it to go back to the built-in prompt.

The same facets can then produce a completely different document — a terse risk memo instead of a
character portrait, say. Because the built-in prompt is still used when you have no override, a
witness upgrade that ships an improved default still reaches you; once you override, your file is
never touched by an upgrade.

## Reading the archive

Humans read the **narrative**; agents read the **structured** data. Over MCP:

- `get_profile(lens)` — the narrative profile (prose); omit `lens` for the unified portrait.
- `get_facets(lens)` — the current structured facets.
- `search_observations(query, lens)` — local vector search over observations.
- `record_observation(...)` — an in-session agent writes a decision-aware observation directly
  (passed through verbatim), capturing context a later reviewer would miss.
- `delete_observation(obs_id)` — prune a wrong observation.

---

[← back to the README](../README.md)
