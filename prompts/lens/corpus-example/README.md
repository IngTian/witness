# corpus-example — a lens for a NON-person corpus

Every other lens witness ships watches a *person*. This one watches a **subject that is not you**:
market commentary, tracking how the regime state changes over time.

Copy it as the starting point for any document corpus — research notes, a knowledge base, incident
reports, meeting logs. The shape transfers; only the dimensions and the prose change.

```sh
cp -R "$CLAUDE_PLUGIN_ROOT/prompts/lens/corpus-example" ./regime   # edit for your domain
witness lens register regime ./regime
witness lens enable regime
```

If your archive is *only* a corpus (no person tracking), start it with no person lens at all:

```sh
WITNESS_NO_DEFAULT_LENS=1 witness doctor    # first open decides; the built-in lens is never seeded
```

Then feed it records and read the result:

```sh
witness ingest --file news.ndjson     # {"text":"…","id":"n1","session":"2026-01","ts":"2026-01-05"}
witness facets regime                 # the current state, with confidence + provenance
witness profile regime                # the narrative brief
```

## What makes this lens different from the person lenses

**`extract.md` names the subject explicitly** — "notice claims about *the market regime* … not about
any person, author, or writing style." Without that, a model reading market prose will happily start
characterizing the *analyst*, because that is the more natural reading of "notice things about the
subject of this text."

**The dimensions are states, not traits.** `rates`, `inflation`, `growth`, `risk_appetite`,
`positioning` are things that have a *current value which changes*. That is what makes the
bi-temporal layer earn its keep: when the regime flips, the old value closes with a date and the new
one opens, so you get a history rather than a replacement.

**`review.md` is explicit about what counts as a change.** The instruction is to set
`contradicts_prior` only for a *sustained* change of state, not a single noisy print — and that
absence of evidence is not contradiction. Getting this wrong in either direction is the main failure
mode: too eager and every data release rewrites history, too shy and a real regime change never
records.

**`key` must be a stable slug.** `policy_path` reviewed in January and again in June is one facet with
two dated versions. A key that drifts (`policy_path_jan`, `jan_policy`) produces two unrelated facets
and no history at all.

## Verified working

This lens was run end to end before shipping: 7 market-news records → 22 observations → 13 facets,
in an archive with no person lens enabled. Then a hawkish→dovish regime flip closed **five** facets
and opened their replacements, e.g.:

```
inflation/core_trend
  "Disinflation has broken rather than paused: core CPI reaccelerating at 3.8%…"  valid_to 2026-06-…
  "Core is disinflating persistently rather than reaccelerating — 2.1% y/y…"      (current)
```

The generated brief also flagged that its own earlier sequencing thesis had been falsified by the new
data — which is the point of keeping the history instead of the latest snapshot.

## Also worth knowing

The **summary prompt** is person-shaped by default too. If the narrative reads like it is describing a
personality rather than your subject, override it — one file, no registration:

```sh
mkdir -p "$(witness doctor | awk '/data root/{print $3}')/summarize"
$EDITOR "$(witness doctor | awk '/data root/{print $3}')/summarize/unified.md"
```

An archive with no override keeps receiving improved defaults on upgrade; once you override, your file
is never touched.
