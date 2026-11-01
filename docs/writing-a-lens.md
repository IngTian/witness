# Writing a lens

A **lens** is how you tell witness what to notice. The one it ships tracks a person's growth; a
lens you write can track anything that has a state which changes — a research area, a market
regime, an incident log, a knowledge base.

This is the main extension point. Nothing in the engine is person-specific: records in,
observations, dated facets, narrative out. Only the prompts decide what the subject is.

## The loop

Writing a lens is prompt-tuning, and there is a command for iterating without touching your
archive:

```sh
cp -R "$CLAUDE_PLUGIN_ROOT/prompts/lens/corpus-example" ./regime   # start from a working lens
$EDITOR regime/extract.md                                          # change what it notices

witness lens try ./regime                      # mine REAL sessions through it, write nothing
witness lens try ./regime --review             # ...and preview the facets it would synthesize
```

`lens try` prints the observations the candidate prompt would produce, and with `--review` the
facets too, **without registering anything or writing to the archive**. Edit, re-run, repeat. It
samples your largest sessions by default — deliberately, because the meatiest sessions are the
ones a prompt is most likely to mishandle. `--recent` samples the latest instead, `--sessions N`
changes how many, and `--model` lets you try a different model without editing your config.

When it behaves, register and enable it:

```sh
witness lens register regime ./regime    # copies the definition into your store
witness lens enable regime               # starts running it on every new session
witness lens backfill regime             # optional: re-mine your whole history through it
```

`register` stores a **copy**, so editing `./regime` afterwards changes nothing until you
re-register. `enable` is a separate switch. `backfill` is what you want if you care about the
history you already have — a newly enabled lens otherwise only sees new sessions.

## The three files

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

## Overriding the summary prompt

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

## Dimensions: traits or states?

This is the choice that decides whether the bi-temporal layer earns its keep.

A **person** lens uses traits — `proof_rigor`, `independence`, `confusion_tolerance`. These
accumulate evidence and shift slowly.

A **corpus** lens should use **states** — `rates`, `inflation`, `risk_appetite`. Things that have a
current value which *changes*. That is where closing a value with a date and opening a new one
actually buys you something: when the regime flips, you get a history instead of a replacement.

If your dimensions are all nouns that could never "flip", consider whether you want a lens at all,
or just search.

## The mistake everyone makes first

**A corpus lens must name its subject explicitly.** Told to "notice things about the subject of this
text", a model reading market commentary will cheerfully start characterising the *analyst* —
because that is the more natural reading. The shipped corpus example says it outright: "notice
claims about *the market regime* … not about any person, author, or writing style."

## A worked example

`prompts/lens/corpus-example/` is a `regime` lens over market commentary, and it has been run end
to end on real data. In an archive with **no** person lens enabled at all, 7 news records became 22
observations and 13 facets. Then the regime flipped hawkish → dovish:

```
inflation/core_trend
  "Disinflation has broken rather than paused: core CPI reaccelerating…"   valid_to 2026-06-…
  "Core is disinflating persistently rather than reaccelerating…"          (current)
```

Five facets closed and reopened that way in a single review, and the generated brief flagged that
its own earlier thesis had been falsified by the new data.

To run a corpus-only archive with no person tracking at all:

```sh
WITNESS_NO_DEFAULT_LENS=1 witness doctor   # the first open decides; the built-in lens is never seeded
witness ingest --file news.ndjson          # {"text":"…","id":"n1","session":"2026-01","ts":"2026-01-05"}
witness facets regime                      # current state, with confidence and provenance
witness profile regime                     # the narrative brief
```

See [`prompts/lens/corpus-example/README.md`](../prompts/lens/corpus-example/README.md) for what
changes when the subject stops being a person.

## Inspecting what you registered

```sh
witness lens list                   # registered lenses and whether each is enabled
witness lens show regime            # its settings
witness lens show regime --prompts  # the full extract/review text, as stored
```

`show --prompts` is worth knowing about: `register` snapshotted the files, so this is the only way
to see what witness is *actually* running, as opposed to what is in your working directory.

---

[← back to the README](../README.md) · [How it works](how-it-works.md) · [Commands](cli.md)
