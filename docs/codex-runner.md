# Codex runner

The Codex runner distills any captured source through `codex exec`. It uses a
separate login in `<archive>/runtime/codex/home`; it never copies the login in
your coding home. Capture still reads `WITNESS_CODEX_HOME`, `CODEX_HOME`, or
`~/.codex` as described in [Codex capture](codex-capture.md).

## Set up

Use Codex CLI **0.169.0** (including its prereleases). Other CLI versions are
refused until their tool contract is verified. The live checks below used
`0.169.0-alpha.11.1` on Windows x64.

```sh
witness auth codex
# Complete the device-code sign-in, then:
witness wire codex
witness config set mine_model <model>
# Optional; otherwise review uses mine_model:
witness config set review_model <model>
```

Choose a bare Codex model slug available to your account, without `provider/`.
At a terminal, wire asks for a missing model. Without a terminal it prints the
configuration command. When another default runner exists, wire asks before
switching it; `witness config set runner codex` chooses it explicitly. Before
login, wire still installs capture hooks and leaves the runner unchanged.

An exported `CODEX_API_KEY` overrides the saved ChatGPT login. `OPENAI_API_KEY`
and a coding session's access token are not inherited. The child gets only
witness's Codex home; the parent environment and capture importer stay unchanged.
For GUI-launched workers, set environment variables where that application
inherits them.

The binary is found on PATH or through `WITNESS_CODEX_BINARY`. On macOS witness
also checks `/Applications/ChatGPT.app/Contents/Resources/codex-cli/bin/codex`
and `/Applications/Codex.app/Contents/Resources/codex`. If the desktop CLI lives
elsewhere, set `WITNESS_CODEX_BINARY` to that executable's absolute path.

## Isolation and limits

Every call uses a new empty working directory outside git, ephemeral execution,
ignored user config/rules, disabled hooks, zero project-document bytes and
`WITNESS_WORKER=1`. `CODEX_SQLITE_HOME` is stripped. The witness instruction is a
separate developer message via `model_instructions_file`; the fenced corpus is
stdin. There is no coding-agent-prompt fallback: a rejected instruction fails the
call rather than silently weakening it.

Feature switches remove configurable tools. A witness-written model catalog
also removes `apply_patch`, code-mode `exec`/`wait`, shell, search and experimental
tools such as clock. The API wire contract checks the actual outgoing request,
including developer messages, rather than trusting those config settings.
Unexpected tool events invalidate the extraction.

A filesystem lock serializes catalog access, login and model calls for this
login across processes, including `lens try`. It is released on process death.
Each call, including lock waiting, has a 20-minute deadline; timeouts report
`context.DeadlineExceeded` and caller cancellation remains cancellation.

Known future usage resets pause only calls using this Codex plan. The reset is
stored in UTC, shown in `witness status`, and schedules a
worker wakeup. After the reset one call probes before waiting calls resume.
The child gets `TZ=UTC`; on Windows Codex's clock uses the system timezone
despite that variable, so witness parses that wall clock before converting to UTC.
Unknown or stale reset times, missing plan access and exhausted API credit are
ordinary errors with the existing retry/backoff behavior. OpenCode's free-model
pause remains independent.

Runner calls use the same Codex plan allowance as coding calls. API-key calls
use API billing. See [Codex pricing](https://learn.chatgpt.com/docs/pricing).
Data use follows your account's plan and settings: personal accounts follow
their model-training preference; Business, Enterprise, Edu and API content is
not used for model improvement by default. See
[OpenAI's data-use explanation](https://help.openai.com/en/articles/5722486-how-your-data-is-used-to-improve-model-performance).
`--ephemeral` controls local session persistence, not those account terms.

## Verification

The ordinary suite uses subprocess stand-ins and never calls a model. To check
the real CLI against a local fake endpoint (dummy API key; no account needed):

```sh
CGO_ENABLED=0 go test -tags codexcontract -run TestContractToolFreeWire -v ./internal/platform/codex
```

The contract covers `gpt-6.1-sol` (Responses Lite) and `gpt-5.5` (ordinary
Responses): zero tool definitions and no extra developer instruction/tool-schema
messages, with the witness system and corpus in their separate channels.

Live ChatGPT checks on November 16, 2026, Windows x64:

| Probe | Result |
| --- | --- |
| A: replaced instruction | Exit 0, `WITNESS-PROBE-OK` |
| B: feature switches alone | Exit 0; still invoked `list_mcp_resources` and `list_mcp_resource_templates` |
| C: switches + stripped catalog | Exit 0; no tool events or file canary |
| D: same on `gpt-5.5` | Unavailable in this login's live catalog; not run live |
| E: complete invocation | Exit 0, `WITNESS-PROBE-OK`, no tool events |

The production runner also passed `TestContractLiveLogin` using the independent
login and a synthetic prompt. To repeat it, set `WITNESS_CODEX_LIVE_ROOT` to the
archive's **runtime directory**, and `WITNESS_CODEX_LIVE_MODEL` to a model in that
login, then run `go test -tags codexcontract -run TestContractLiveLogin -v
./internal/platform/codex`. This makes one real model call.
