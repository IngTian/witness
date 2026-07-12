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
make build-all    # cross-compile darwin+linux+windows, amd64+arm64
make package-windows     # self-contained Windows zips (exe + prompts + model)
```

Before finishing a change, run `make fmt vet test` and expect all green.

**macOS gotcha:** always `make build` (or `go build -o bin/...`) — do **not**
`cp` a built arm64 binary to another path and run it. macOS invalidates the
ad-hoc code signature on copy and the copy dies with SIGKILL (137). Build in
place. (The Windows install path *does* copy the binary into `%LOCALAPPDATA%`
— that's fine, Windows has no such signature, and `install_windows.go` guards a
self-copy with `os.SameFile` + writes atomically via temp+rename.)

## Architecture (where things live)

- `cmd/witness/` — 15-line entry point; calls `commands.Run()`.
- `cmd/commands/` — one cobra command per file (capture, session, worker, mcp,
  install, doctor, profile, facets, observations, review, lens, import, distill,
  cleanup). Shared helpers in `cli.go`; terminal styling in `style.go`.
- `internal/store/` — SQLite data model + the filesystem/DB-as-queue. `raw`
  (L0), `observations` (L1), `facets`/`facet_versions` (L2, bi-temporal),
  `progress` (distill watermark), `staged`, `meta`. `MaxOpenConns(1)` + WAL +
  `busy_timeout` is the deliberate concurrency model. `resolveRoot()` picks the
  data dir via `dataDirNames` (adopt-in-place, see invariants below).
- `internal/distill/` — the worker: mines L0→L1 per lens, reviews L1→L2, and
  regenerates L4 via a headless runner (`claude -p` or `opencode serve`).
- `internal/platform/{claude,opencode}/` — the per-runtime capture/import
  adapters. `internal/platform/opencode/plugin/` embeds the OpenCode plugin JS.
  (`internal/platform/platform.go` holds the cross-runtime constants + types.)
- `internal/bundle/` — resolves bundled-asset dirs (`prompts/`, the embedding
  model) relative to the binary via `os.Executable()` when no env var points at
  them, so a copied/installed binary self-locates its assets.
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
  hooks. `WITNESS_WORKER=1` short-circuits witness inside that subprocess. Both
  the Unix shim `hooks/witness.sh` and `commands.Run()` check it — and on Windows
  (exec-form hooks, no shim) `commands.Run()` is the *only* guard, so keep it
  intact.
- **Install is per-platform (`resolveClaudeInstall`, GOOS-split).** Unix wires
  shell-form hooks through the `hooks/witness.sh` shim (in-repo working copy).
  Windows has no guaranteed shell: `install_windows.go` COPIES the binary +
  `prompts/` + model into `%LOCALAPPDATA%\witness` and wires **exec-form** hooks
  (`{command: <exe>, args: [...]}`) pointing at the installed exe. Keep the two
  paths separate — don't unify onto the shim.
- **Hook name contract.** Installed hooks invoke `witness <session-start|capture|
  session-end>` (shell form) or the same tokens as exec-form `args`; the binary
  must keep cobra commands of exactly those names, and `isWitnessEntry` must
  recognize *only* our own hooks (exact `witness` basename + our tokens) so a
  re-install never strips a foreign hook. `cmd/commands/hookcontract_test.go` +
  `install_test.go` lock these — run them after touching install wiring or
  command names.
- **Lenses are global** (never repo-scoped): a cloned/hostile repo cannot inject
  a prompt into your archive. Nothing is read from a repo directory.
- **Data dir adopts, never moves.** `dataDirNames = {"witness","claude-witness"}`
  (preferred first, legacy appended). `resolveRoot()` uses an existing dir under
  any listed name IN PLACE and only creates `witness` on a fresh machine — the
  repo renamed but the data dir must never orphan an existing archive. A future
  rename just prepends a name; never rewrite these literals as a "cleanup".

## Conventions

- Match the surrounding style: dense, purposeful comments that explain *why*
  (invariants, tradeoffs), not *what*. Package docs at the top of key files.
- Human CLI output may use `cmd/commands/style.go` helpers (color/glyphs) — but
  keep it TTY + `NO_COLOR` aware, and never let styling touch `--json` output.
- New behavior needs a test. The CC capture path and the hook name-contract are
  guarded (`internal/platform/claude/capture_test.go`,
  `cmd/commands/hookcontract_test.go`) — keep them passing.
- Never commit anyone's actual archive data (see `.gitignore`); the repo ships
  the framework, schema, and prompts only.
