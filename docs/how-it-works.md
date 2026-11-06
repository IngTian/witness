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

After a portrait is written, a background step links each of its sentences to the traits it rests on. Each link is marked **direct** (the trait fully states the sentence, or one of its claims) or **partial** (it covers only part of a claim, or is related). The step also records a few keywords for the portrait. The links are stored with the archive, tied to the exact portrait text, and refresh on the next worker run when the portrait changes. The portrait itself is never changed by this.

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

So a confidence you saw last month may read lower today even though nothing about that facet
changed — the sessions in between were chances to confirm it, and none did. That is working
correctly. Every place that shows a confidence shows this current value: `witness facets
<lens>`, the agent-facing `get_facets` tool, and the portrait. Only `--json` reports the stored
value as of the facet's last confirmation, because that is the record the decay is computed from.
`witness facets <lens> --expand <dimension/key>` unfolds everything that was merged into a facet.

Every change witness makes to a facet's confidence — a review that raised, lowered or rewrote
it, the long-arc pass, or a compaction that merged it with other facets — is recorded with the
date and what caused it. The history starts at the upgrade (earlier changes were overwritten in
place and cannot be recovered), lives in the archive, and travels with `witness export <path>
--all`. `witness merge` does not carry the other archive's history: facets that arrive from it
start without one, and confidence it adopts from them is not recorded.

### Lenses

Every observation/facet carries a **lens** tag:

- **`default`** — global, runs on every session, cross-domain. This is the part no single-domain
  tracker can be: it sees that "diagnoses gaps precisely" fires in math *and* coding *and* career.
- **registered lenses** (e.g. `math`) — domain-specific lenses you **register once** and **enable
  globally**. `witness lens register math ./math/` adds the definition (a directory) to a central
  registry; `witness lens enable math` makes it run on every session (alongside `default`). Lenses
  are shared, not tied to any repo, so the same `math` lens covers all your math work.

### Writing your own lens

A lens is a directory of three files — settings plus an extract prompt and a review prompt — and
writing one is how witness gets used for anything other than the shipped person lens. That has its
own guide, including the no-write iteration loop (`witness lens try`) and a worked non-person
example: **[Writing a lens](writing-a-lens.md)**.

## Reading the archive

Humans read the **narrative**; agents read the **structured** data. Over MCP:

- `get_profile(lens)` — the narrative profile (prose); omit `lens` for the unified portrait.
- `get_facets(lens)` — the current structured facets.
- `search_observations(query, lens)` — local vector search over observations.
- `record_observation(...)` — an in-session agent writes a decision-aware observation directly
  (passed through verbatim), capturing context a later reviewer would miss.
- `delete_observation(obs_id)` — prune a wrong observation.

## Seeing it

`witness view` writes a self-contained HTML snapshot with two pages: **Overview** (your activity
calendar, overall portrait, and the notebook portraits) and **How you've changed** (each notebook's
topics and traits, with their trend line, confidence history, and evidence). Open it in a browser.
It is a snapshot, not a live view — run it again to refresh. It never regenerates your portraits: if
you want those updated first, run `witness profile`.

The file lives at `<data dir>/view.html` by default, or at the path you give with `--out <path>`. It
contains your archive's text and is written with owner-only permissions (mode 0600). Nothing listens
on the network.

---

[← back to the README](../README.md)
