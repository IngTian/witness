# AGENTS.md

Guidance for AI coding agents (and humans) working in this repo. Claude Code
reads this via `@AGENTS.md` in [CLAUDE.md](CLAUDE.md); other tools may read it
directly.

## What this is

`witness` is a single pure-Go binary that captures Claude Code and OpenCode
sessions into a **person-centric growth archive** (raw turns → observations →
facets → narrative profile). It is a **pure capture+serve tool** — no coaching,
no injection into sessions. See [README.md](README.md) for the full model.

## Build, test, run

Everything is `CGO_ENABLED=0` (single static binary, no cgo). Use the Makefile:

```sh
make build        # compile to bin/witness-<os>-<arch> (with version ldflags)
make test         # CGO_ENABLED=0 go test ./...
make vet          # CGO_ENABLED=0 go vet ./...
make fmt          # gofmt -w internal cmd
make doctor       # build + run the embedder/model/config health check
make install claude      # (or: opencode) wire hooks/plugin + MCP + bind runner
make build-all    # cross-compile darwin+linux, amd64+arm64
```

Before finishing a change, run `make fmt vet test` and expect all green.

**macOS gotcha:** always `make build` (or `go build -o bin/...`) — do **not**
`cp` a built arm64 binary to another path and run it. macOS invalidates the
ad-hoc code signature on copy and the copy dies with SIGKILL (137). Build in
place.

## Architecture (where things live)

- `cmd/witness/` — 15-line entry point; calls `commands.Run()`.
- `cmd/commands/` — one cobra command per file (capture, session, worker, mcp,
  install, doctor, profile, facets, observations, review, lens, import, distill,
  cleanup). Shared helpers in `cli.go`; terminal styling in `style.go`.
- `internal/store/` — SQLite data model + the filesystem/DB-as-queue. `raw`
  (L0), `observations` (L1), `facets`/`facet_versions` (L2, bi-temporal),
  `progress` (distill watermark), `staged`, `meta`. `MaxOpenConns(1)` + WAL +
  `busy_timeout` is the deliberate concurrency model.
- `internal/distill/` — the worker: mines L0→L1 per lens, reviews L1→L2, and
  regenerates L4 via a headless runner (`claude -p` or `opencode serve`).
- `internal/runtimes/{claude,opencode}/` — the per-runtime capture/import
  adapters. `internal/runtimes/opencode/plugin/` embeds the OpenCode plugin JS.
- `internal/{embed,vector,lens,mcp}/` — embeddings (pure-Go GoMLX), cosine
  search, lens loading, the MCP server.

## Runtime model (important invariants)

- **Two runtimes, one store.** Claude Code and OpenCode both feed the same L0.
  Rows are namespaced by session id (OpenCode uses an `opencode:` prefix); meta
  keys are namespaced (`opencode_*` vs CC's `worker_*`/`review_*`). Neither can
  corrupt the other's data.
- **The runner is global.** One `runner` (in `config.toml`) distills every
  session regardless of source. `witness install <target>` binds it.
- **Capture must never break a session.** `capture`/`session-start`/`session-end`
  are best-effort: they log failures but always exit 0. Do not make them return
  errors on a bad hook payload.
- **Recursion guard.** The worker runs `claude -p`, which would re-fire the
  hooks. `WITNESS_WORKER=1` short-circuits witness inside that subprocess (both
  the shim `hooks/witness.sh` and `commands.Run()` check it). Keep it intact.
- **Hook name contract.** Installed hooks invoke `witness <session-start|capture|
  session-end>`; the binary must keep cobra commands of exactly those names.
  `cmd/commands/hookcontract_test.go` locks this — run it after touching install
  wiring or command names.
- **Lenses are global** (never repo-scoped): a cloned/hostile repo cannot inject
  a prompt into your archive. Nothing is read from a repo directory.

## Conventions

- Match the surrounding style: dense, purposeful comments that explain *why*
  (invariants, tradeoffs), not *what*. Package docs at the top of key files.
- Human CLI output may use `cmd/commands/style.go` helpers (color/glyphs) — but
  keep it TTY + `NO_COLOR` aware, and never let styling touch `--json` output.
- New behavior needs a test. The CC capture path and the hook name-contract are
  guarded (`internal/runtimes/claude/capture_test.go`,
  `cmd/commands/hookcontract_test.go`) — keep them passing.
- Never commit anyone's actual archive data (see `.gitignore`); the repo ships
  the framework, schema, and prompts only.
