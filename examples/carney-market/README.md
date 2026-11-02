# Example: Mark Carney — a market lens

Reads Canada's Prime Minister (and former central banker) as a market variable: what he does to
trade, to domestic economic policy, and to monetary conditions — and, from the tradeoffs he makes,
**what he actually optimizes for**.

**Dimensions:** `objective_function` · `us_trade` · `domestic_policy` · `monetary_conditions` · `market_reaction`

Pairs with [`trump-market`](../trump-market): the same tariff events seen from the receiving end.

## What it produced

`sample-output/` is real, unedited output from a 188-article corpus: **392 observations → 52
facets**. Facets are deliberately few — the review collapses each theme into ONE key so its value
evolves over time instead of spawning near-duplicates.

- [`sample-output/facets.md`](sample-output/facets.md) — every current facet by dimension, with confidence and citation counts
- [`sample-output/profile.md`](sample-output/profile.md) — the profile witness synthesized from them

A sense of the register: the profile opens with the structural fact that governs everything —
Canada sends 76% of its goods exports to the U.S. while receiving 17% of U.S. exports, so
"this asymmetry eliminates policy optionality" — then contrasts campaign rhetoric of "rupture"
against the revealed strategy of accepting deeper integration to preserve CUSMA access, prices
the trigger (Q2 2025: 1.6% GDP contraction, 15.7% export collapse), and characterises the
retaliation doctrine as "instrumental, not principled" ($30B deployed, most capacity held in
reserve, tariffs removed once pass-through inflation appeared).

Facets follow a **position → mechanism → what would change it** shape, so each one is usable
rather than merely descriptive.

## Run it

The articles themselves are not in this repository — they are other outlets' copyrighted work.
[`sources.tsv`](sources.tsv) lists all 188 (date, outlet, title, link), enough to re-gather them, or
to build your own corpus the same way (see below). Each article is one line of NDJSON:

```json
{"text": "<title, then the article text>", "id": "<its URL — re-ingesting the same id is a no-op>", "ts": "2025-03-27"}
```

From a clone of this repository, in an archive of its own (not your personal one):

```sh
export WITNESS_HOME=./carney-archive
WITNESS_NO_DEFAULT_LENS=1 witness doctor     # the first open decides: no person lens in this archive
witness lens register carney-market examples/carney-market/lens
witness lens enable carney-market
witness config set mine_model haiku          # a fast model is plenty; see below
witness config set review_model haiku
witness ingest --file corpus.ndjson          # distillation starts in the background
witness status                               # wait for 0 pending
witness worker review                        # fold the last observations into facets now
witness facets carney-market
witness profile carney-market
```

Distillation is roughly one model call per article, then the review folds observations in bounded
windows — so pin a fast model before a run this size. Left unpinned, a review window inherits
whatever heavy model your runner defaults to and can exceed the per-call timeout. On today's
witness, `haiku` pulled four observations from a single 2025 budget article — each naming its
channel, its numbers, and a quote.

Expect some articles to yield nothing. A 2024 Bank of Canada decision from before he was Prime
Minister returns `[]` — that is the lens working, not failing. `witness lens try carney-market`
previews what the lens makes of any session without writing anything.

## The corpus is the lever

The corpus was **hand-curated, and that is the point.** An earlier version of this example used a
corpus gathered by querying the subject's *name*; it returned political coverage, and the distilled
profile read like a newspaper column — accurate, but useless for anticipating economic effects.
This one was gathered by querying *market topics* instead — Bank of Canada rate decisions and the
Monetary Policy Report, tariff and USMCA developments, budget and deficit coverage, the loonie and
bond yields — then filtered for economic substance. Its sources are financial and primary press
(Bank of Canada, Reuters, Financial Post, BNN Bloomberg, Morningstar, Globe and Mail, pm.gc.ca),
balanced across four topic families and across 2024–2026.

Same subject, same tool, same lens discipline. **The corpus decided the output.**

## Make it yours

Copy `lens/`, edit `lens.json`'s dimensions and the guidance prose in `extract.md` / `review.md`,
and register your copy. Two rules worth keeping, because they are what make this lens behave:

1. `extract.md` requires an **economic channel** for every observation — name the rate, price,
   sector, dollar amount, or trade flow it moves, or return `[]`. This is what keeps political
   framing out.
2. `review.md` asks for **the position, its mechanism and numbers, and what would change it** —
   rather than asking the model to find the tension in the story, which reliably promotes
   conflict to the headline.

Leave the embedded JSON schemas exactly as they are; witness parses them.
