# Example: Donald Trump — a market lens

Reads Trump as a **source of market shocks**: the tariffs he imposes and withdraws, the pressure
he puts on the Fed, what markets actually price in response, the uncertainty itself as an
economic variable — and, from the tradeoffs he makes, **what he optimizes for**.

**Dimensions:** `objective_function` · `trade_actions` · `monetary_pressure` · `market_reaction` · `policy_uncertainty`

Pairs with [`carney-market`](../carney-market): the same tariff events seen from the receiving
end. Reading the two side by side is the point — one is about generating the shock, the other
about absorbing it.

## What it produced

`sample-output/` is real, unedited output from a 200-article corpus: **558 observations → 83
facets**.

- [`sample-output/facets.md`](sample-output/facets.md) — current facets by dimension, with confidence and citation counts
- [`sample-output/profile.md`](sample-output/profile.md) — the profile witness synthesized from them

The profile's thesis is the self-inflicted constraint: tariff escalation to a 17–18% average
(highest since 1934) plus the Iran shock produced 4.2% trailing inflation — "the outcome the
administration was simultaneously working hardest to prevent" — forcing the Fed into hikes toward
3.9%. It then identifies the reaction function markets named for themselves, *TACO* ("Trump Always
Chickens Out"): a 30% EU tariff threat spiked Treasury yields ~80bp intraday, he reversed within
hours, and equities rallied. The second-order insight is the tradeable one — markets stopped
repricing to announcements at all (the July 2025 EU deal moved the S&P +0.02%), so headline tariff
rates became unreliable signals and investors price the *expected negotiated* outcome instead.
It also separates the reversible from the irreversible: China's soybean shift to Brazil, the EU
opening CPTPP talks, and relocated supply chains persist even if tariffs do not.

A known wrinkle: eight themes appear under two dimensions each (e.g. `average_tariff_burden` in
both `trade_actions` and `market_reaction`). A tariff burden genuinely is both an action and a
market effect, so this is lens ambiguity rather than a defect — but it means a few themes are
read twice. `witness compact` will not merge them: it only groups facets within one dimension and
refuses a cross-dimension merge. The lever is the lens's dimension rules — tighten what each
dimension in `extract.md` and `review.md` owns, re-register, then re-mine with
`witness lens backfill trump-market --fresh`.

## Run it

The articles are not in this repository — they are other outlets' copyrighted work.
[`sources.tsv`](sources.tsv) lists all 200 (date, outlet, title, link). The commands are the same as
[`carney-market`'s](../carney-market#run-it), with this lens:

```sh
export WITNESS_HOME=./trump-archive
WITNESS_NO_DEFAULT_LENS=1 witness doctor
witness lens register trump-market examples/trump-market/lens
witness lens enable trump-market
witness config set mine_model haiku
witness config set review_model haiku
witness ingest --file corpus.ndjson
witness status                               # wait for 0 pending
witness worker review
witness facets trump-market
witness profile trump-market
```

A corpus this size runs for hours — roughly one model call per article, then the review windows.

## The corpus is the lever

The corpus was **hand-curated, and that is the point.** Trump generates enormous news volume, most
of it politically framed and useless for anticipating market effects. This one was gathered by
querying *market topics* — tariff actions and effective dates, Fed and rate pressure, measured
index/yield/dollar moves, policy reversals and deadlines, and fiscal/inflation data — then filtered
for economic substance and balanced across those five families and across 2024–2026.

A corpus assembled by querying his *name* would return the news cycle. Assemble yours on purpose.

## Make it yours

Copy `lens/`, edit `lens.json`'s dimensions and the guidance prose in `extract.md` / `review.md`,
and register your copy. Three rules carry most of the quality:

1. `extract.md` demands an **economic or market channel** for every observation — name the rate,
   the index move, the dollar amount, the capex decision, or return `[]`. This is what keeps the
   news cycle out.
2. `review.md` asks for **the position, its mechanism and numbers, and what would change it** —
   not for "the tension in the story", which reliably promotes conflict to the headline.
3. `review.md` also treats **a reversal under market pressure as the most informative event in the
   file** — when he backs down, what forced it, which market, and how fast. That is the reaction
   function a reader most needs.

Leave the embedded JSON schemas exactly as they are; witness parses them.
