# witness — a distillation engine that keeps the history of how things changed.

[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)
[![npm](https://img.shields.io/npm/v/@witness-ai/opencode?logo=npm&label=%40witness-ai%2Fopencode)](https://www.npmjs.com/package/@witness-ai/opencode)
![Go](https://img.shields.io/badge/Go-1.25-00ADD8?logo=go&logoColor=white)
![Single binary](https://img.shields.io/badge/single%20binary-CGO__ENABLED%3D0-informational)
![Runtimes](https://img.shields.io/badge/runtimes-Claude%20Code%20%C2%B7%20OpenCode-8A2BE2)

[![witness MCP server](https://mcprush.com/badge/witness-mcp/card.svg)](https://mcprush.com/to/witness-mcp?ref=badge)

**witness turns a stream of text into a dated record of how its subject changed.** Feed it your AI
coding sessions and it tracks how *you* think and work. Feed it market commentary, research notes, or
any document stream and it tracks how *that* changed instead. One pure-Go binary, a local SQLite
archive plus plain markdown, served to your agent over MCP.

The thing it is built around is **change over time, with provenance**. When an attribute's value
changes, witness does not overwrite it — it closes the old value with a date and opens a new one, and
records which source records drove the shift. So the archive answers *"how did this get here"*, not
just *"what is true now"*.

> *"Aah, you were at my side, all along.*
> *My true mentor...*
> *My guiding moonlight..."*
> — Ludwig, the Holy Blade

## Two ways people use it

**1. Watch your own growth as you work with AI.** Install it into Claude Code, OpenCode or Codex and it
captures your sessions in the background — no prompting, no ritual. It distills how your thinking,
workstyle, habits, and blind spots shift over months, with evidence for every claim. This is the
setup witness ships configured out of the box.

It is **reflection-oriented, not clone-oriented**: the point is to let your agent understand you, and
to leave you a re-readable record of how you thought and grew. It is a **pure tool, not a coach** —
it captures, structures, and serves. Building a coach on top ("you've done this three times…") is
left to other projects that read its output.

**2. Build a change-history over any text corpus.** Write a lens — a prompt describing what to notice
and which dimensions to track — point `witness ingest` at NDJSON records, and the same four-stage
engine runs. Nothing about the machinery is person-specific: raw records → observations → dated
facets → narrative.

A worked example, run end to end while writing this section. A `regime` lens over market commentary,
in an archive with **no** person lens enabled at all: 7 news records became 22 observations and 13
facets. Then the regime flipped hawkish→dovish, and the archive recorded it as history rather than
replacing it:

```
inflation/core_trend
  "Disinflation has broken rather than paused: core CPI reaccelerating…"   valid_to 2026-06-…
  "Core is disinflating persistently rather than reaccelerating…"          (current)
```

Five facets closed and reopened that way in one review. The generated brief even flagged that its own
earlier sequencing thesis had been **falsified** by the new data — which is the whole point of keeping
the history instead of the latest snapshot.

**Honest caveat:** the engine is general, but everything witness *ships* is person-shaped — the
built-in `default` lens, the example lens, and the summary prompts all say "notice things about the
person". For a non-person corpus you write your own lens (three files) and, if you want, override the
summary prompt with one file. There is no market-lens or research-lens preset yet; the machinery is
domain-agnostic, the batteries included are not.

## Is this you?

witness is the answer if you've ever wanted to:

- **Remember what you learned across your AI coding sessions** — Claude Code, OpenCode, Codex — instead of losing it when the context window resets.
- Have a **second brain / AI memory** for your thinking, workstyle, and habits, queryable by your agent.
- **Track how you grow as a developer over time** — the traps you fall into and climb out of — with provenance for every recorded change.
- Keep a **journal / retrospective** of how you thought and changed, re-readable months later.
- **Distill a corpus you care about** — market news, a knowledge base, research notes, meeting logs — into a dated history of how its state changed, not a pile of summaries. Feed it NDJSON with `witness ingest`.
- Query any of the above **from your agent**, over MCP, instead of re-explaining context every session.

It will not tell you much on day one. witness measures *change*, so it needs weeks of material
before there is anything to show — and it captures and tracks rather than advising or grading.

**Contents:** [What it produces](#what-it-produces) · [Install](#install) · [Connect your tools](#connect-your-tools) · [Using it](#using-it) · [Your data](#your-data-is-yours) · [How it works](docs/how-it-works.md) · [Commands & config](docs/cli.md)

## What it produces

Say a session contains this exchange:

> **you:** the migration keeps failing on prod but passes locally — I'll just run it by hand and move on
>
> **you:** …wait, what's actually *different* about prod? let me diff the two schemas before I touch anything

It's captured verbatim, then mined into one evidence-anchored noticing:

```
[thinking] Caught the urge to hand-patch around a failure and redirected to isolating the
           prod/local difference before acting.
  evidence: "run it by hand and move on" → "what's different about prod? diff before I touch anything"
  poignancy: 6    lens: default
```

After several such moments, an evolving attribute — **with its history kept**:

```
default · thinking · diagnoses_before_acting                        confidence 0.82
  2026-05 → now       Catches the reflex to work around a failure and isolates the
                      mechanism first; gates action on understanding the cause.
  2026-02 → 2026-05   Tended to apply the first workaround that unblocked the task.   (superseded)
```

And the narrative you actually read (`witness profile`):

> You've been converging on a diagnose-first way of working. A few months ago the pattern was to
> reach for whatever unblocked the task; now you routinely catch that urge and turn to isolating the
> mechanism before you touch anything…

Nothing is pushed into your sessions. You read it when you want it, or your agent pulls the relevant
facet on demand.

## Install

```sh
git clone https://github.com/IngTian/witness && cd witness
./install.sh claude      # or: opencode, codex
```

The target is required. The script builds the binary, fetches the embedding model (~448MB, once),
provisions the archive, and wires that tool. Idempotent — safe to re-run after a `git pull`.

Wiring more than one is just running it again: `./install.sh codex` after `./install.sh claude`
adds Codex to the same archive.

Or grab a binary from [releases](https://github.com/IngTian/witness/releases). Windows users want
the self-contained `.zip` — see [Windows install](docs/windows.md).

OpenCode users can install from npm instead: `npm i -g @witness-ai/opencode` — see
[OpenCode support](docs/opencode.md).

## Connect your tools

witness captures from four sources at once, into one archive and one set of lenses. Sessions are
namespaced per source, so none can corrupt another's data.

| source | setup |
|---|---|
| **Claude Code** | `witness wire claude` |
| **OpenCode** | `witness wire opencode` |
| **Codex** | `witness wire codex`, then approve the hooks with `/hooks` inside Codex — see [Codex capture](docs/codex-capture.md) |
| **Files / documents** | `witness ingest --file records.ndjson` — you convert your notes, articles or logs into NDJSON records first; witness owns the record format, not your data |

`witness wire` sets up capture only and never prompts, so it is safe in CI or from a hook.
(`witness install` is a different thing: it provisions a bare archive at a `--path`, for keeping
an independent archive per data source. It does not wire any editor.)

Then just work. Capture runs in the background — no prompting, no ritual. Check it any time with
`witness doctor`.

## Using it

```sh
witness profile              # the narrative, across everything
witness profile math         # one lens
witness facets               # the structured attributes behind it
witness compact              # consolidate a grown-over profile (dry run)
witness doctor               # health check
```

Your agent reads the same archive over MCP — `get_profile`, `get_facets`, `search_observations`.

Everything is per-lens: a **lens** is a prompt pair plus config that decides what gets noticed and
how it's synthesized. The default one tracks personal growth; write your own to track anything else.
See [How it works](docs/how-it-works.md).

## Your data is yours

- **Local by default.** One SQLite file plus markdown under your data dir (`witness doctor` prints
  the path). No telemetry, no account, no cloud key.
- **Local embeddings.** Multilingual (English and Chinese) embeddings run pure-Go on your machine.
- **Distillation uses your own tooling** — your existing Claude Code or OpenCode auth, whichever you
  bound. Nothing goes anywhere you haven't already chosen.
- **Readable and deletable.** Plain SQLite and plain markdown. `witness export --all <dir>` snapshots
  the whole archive; delete the data dir and it's gone.

## Where to look for things

| I want to… | go here |
|---|---|
| install it and connect my editor | [Install](#install) · [Connect your tools](#connect-your-tools) above |
| understand the four layers and what each produces | [How it works](docs/how-it-works.md) |
| know why a confidence number changed on its own | [Confidence, and why it moves](docs/how-it-works.md#confidence-and-why-it-moves-on-its-own) |
| **track something other than myself** — notes, research, a market, a knowledge base | [Writing a lens](docs/writing-a-lens.md) |
| tune what gets noticed, or test a prompt before committing to it | [Writing a lens → the loop](docs/writing-a-lens.md#the-loop) |
| look up a command or a flag | [Commands](docs/cli.md#commands) |
| change the model, cadence, or concurrency | [Configuration](docs/cli.md#configuration) |
| set up Codex capture | [Codex capture](docs/codex-capture.md) |
| set up OpenCode, or install from npm | [OpenCode support](docs/opencode.md) |
| install on Windows | [Windows install](docs/windows.md) |
| back up or move my archive | [`witness export --all <dir>`](docs/cli.md#commands) |
| move to a machine that already has an archive, without replacing it | [`witness merge <dir>`](docs/cli.md#commands) |
| shrink a profile that has grown unwieldy | [`witness compact`](docs/cli.md#commands) |
| know where my data lives and how to delete it | [Your data is yours](#your-data-is-yours) above |
| contribute, or understand the invariants | [AGENTS.md](AGENTS.md) |

## Single binary, no runtime

One self-contained Go binary — no Python, no external services, no vector DB. `CGO_ENABLED=0`, so
it cross-compiles cleanly and has no shared-library surprises.

## License

MIT — see [LICENSE](LICENSE).
