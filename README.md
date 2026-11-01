# witness

[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)
[![npm](https://img.shields.io/npm/v/@witness-ai/opencode?logo=npm&label=%40witness-ai%2Fopencode)](https://www.npmjs.com/package/@witness-ai/opencode)
![Go](https://img.shields.io/badge/Go-1.25-00ADD8?logo=go&logoColor=white)
![Single binary](https://img.shields.io/badge/single%20binary-CGO__ENABLED%3D0-informational)
![Sources](https://img.shields.io/badge/sources-Claude%20Code%20%C2%B7%20OpenCode%20%C2%B7%20Codex%20%C2%B7%20files-8A2BE2)

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
curl -fsSL https://raw.githubusercontent.com/IngTian/witness/main/install.sh | sh
```

Or grab a binary from [releases](https://github.com/IngTian/witness/releases). Windows users want
the self-contained `.zip` — see [Windows install](docs/windows.md).

OpenCode users can install from npm instead: `npm i -g @witness-ai/opencode` — see
[OpenCode support](docs/opencode.md).

## Connect your tools

witness captures from four sources at once, into one archive and one set of lenses. Sessions are
namespaced per source, so none can corrupt another's data.

| source | setup |
|---|---|
| **Claude Code** | `witness install claude` |
| **OpenCode** | `witness wire opencode` |
| **Codex** | manual for now — see [Codex capture](docs/codex-capture.md) ([#179](https://github.com/IngTian/witness/issues/179) tracks making it one command) |
| **Files / documents** | `witness ingest <path>` |

Then just work. Capture runs in the background — no prompting, no ritual. Check it any time with
`witness doctor`.

## Is this for you?

Probably, if you have ever wanted to:

- ask *"am I actually getting better at this, or just busier?"* and get an answer with evidence
- know **when** a habit changed, not just that it did
- hand an agent real context about how you work, instead of re-explaining yourself
- track how a **corpus** evolved — a market thesis, a research area, a body of notes

**Probably not, if** you want a coach, a dashboard, or a productivity score. witness captures and
tracks; it doesn't advise, grade, or nag. It also won't tell you much on day one — it needs weeks of
material before change is visible, because change is the thing it measures.

## Beyond your own sessions

Nothing in the engine is person-specific. Write a lens — a prompt describing what to notice and
which dimensions to track — point `witness ingest` at your records, and the same four stages run.

A worked example: a `regime` lens over market commentary, in an archive with **no** person lens
enabled. 7 news records became 22 observations and 13 facets. Then the regime flipped hawkish →
dovish, and the archive recorded it as history rather than replacing it:

```
inflation/core_trend
  "Disinflation has broken rather than paused: core CPI reaccelerating…"   valid_to 2026-06-…
  "Core is disinflating persistently rather than reaccelerating…"          (current)
```

Five facets closed and reopened that way in a single review, and the generated brief flagged that
its own earlier thesis had been **falsified** by the new data — which is the point of keeping the
history instead of the latest snapshot.

**The honest caveat:** the engine is general, but everything witness *ships* is person-shaped — the
built-in lens, the example lens, and the summary prompts all say "notice things about the person".
For another domain you write your own lens (three files) and optionally override one summary prompt.
There's no market-lens or research-lens preset yet. The machinery is domain-agnostic; the batteries
included are not.

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

## Documentation

| | |
|---|---|
| [How it works](docs/how-it-works.md) | the four layers, lenses, writing your own prompts |
| [Commands and configuration](docs/cli.md) | every command, every config key |
| [OpenCode support](docs/opencode.md) | setup, the isolated-runtime model, npm install |
| [Codex capture](docs/codex-capture.md) | setup, the read-only probe, compatibility limits |
| [Windows install](docs/windows.md) | why Windows differs, and what it copies where |
| [AGENTS.md](AGENTS.md) | architecture and invariants, for contributors |

## Single binary, no runtime

One self-contained Go binary — no Python, no external services, no vector DB. `CGO_ENABLED=0`, so
it cross-compiles cleanly and has no shared-library surprises.

## License

MIT — see [LICENSE](LICENSE).
