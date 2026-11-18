# Codex runner

The Codex runner distills any captured source through `codex exec`. It uses a
separate login in `<archive>/runtime/codex/home`; it never copies the login in
your coding home. Capture still reads `WITNESS_CODEX_HOME`, `CODEX_HOME`, or
`~/.codex` as described in [Codex capture](codex-capture.md).

## Set up

Use Codex CLI **0.169.0** or **0.169.0-alpha.11.1**. Both builds are covered by
the Linux and Windows wire contract. A daily check runs against npm latest
and files an issue when the contract fails or the release is still unpinned.
`witness doctor` checks the binary, version,
login, managed policy and configured models. After a CLI update, use
`WITNESS_CODEX_BINARY` to point witness at an installed supported binary.

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

Set `CODEX_API_KEY` to use API billing. Key-backed calls use a temporary Codex
home. The device login stays in witness's private runtime directory and is
excluded from archive exports. `witness auth codex --logout` removes the saved
runner login. For GUI-launched workers, set environment variables where that
application inherits them.

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
tools such as clock. Bundled skills are disabled. Unexpected tool events
invalidate the extraction.

Codex can load system, MDM and cloud policy even with user config ignored.
Witness queries the CLI for config layers and requirements before sending any
corpus. Nonempty system config, managed layers or requirements, and unknown
policy results block the runner. The check applies to actual policy, independent
of the ChatGPT plan. ChatGPT may add server-side framing.

A filesystem lock serializes catalog access, login and model calls for this
login across processes, including `lens try`. It is released on process death.
Open and Run wait under the caller's context. `doctor` and `wire` report a
busy login separately from invalid configuration. The 20-minute model deadline starts
after the lock and preflight. Generation timeouts report
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
their model-training preference. API content is excluded from model improvement
by default. See
[OpenAI's data-use explanation](https://help.openai.com/en/articles/5722486-how-your-data-is-used-to-improve-model-performance).
`--ephemeral` controls local session persistence, not those account terms.

## Verification

The ordinary suite uses subprocess stand-ins and never calls a model. To check
the real CLI against a local fake endpoint (dummy API key; no account needed):

```sh
CGO_ENABLED=0 go test -tags codexcontract -run TestContractToolFreeWire -v ./internal/platform/codex
```

The contract covers `gpt-6.1-sol` (Responses Lite) and `gpt-5.5` (ordinary
Responses). It requires captured requests for both models, checks top-level
tools and rejects all input items other than the expected messages. A
negative control drops the catalog override for `gpt-6.1-sol` and must
trigger the `additional_tools` assertion.

Live ChatGPT checks on November 16, 2026, Windows x64:

| Probe | Result |
| --- | --- |
| A: replaced instruction | Exit 0, `WITNESS-PROBE-OK` |
| B: feature switches alone | Exit 0; still invoked `list_mcp_resources` and `list_mcp_resource_templates` |
| C: switches + stripped catalog | Exit 0; no tool events or file canary |
| D: same on `gpt-5.5` | Unavailable in this login's live catalog; not run live |
| E: complete invocation | Exit 0, `WITNESS-PROBE-OK`, no tool events |

On November 18, 2026, the runner with policy preflight passed Probe E and a
one-session synthetic drain using Codex CLI `0.169.0` on Windows x64. Both Codex
calls exited 0 with no tool events. The drain produced two observations, and
`witness doctor` exited 0 with the runner models and embedder healthy.

To repeat Probe E, set `WITNESS_CODEX_LIVE_ROOT` to the
archive's **runtime directory**, and `WITNESS_CODEX_LIVE_MODEL` to a model in that
login, then run `go test -tags codexcontract -run TestContractLiveLogin -v
./internal/platform/codex`. This makes one real model call.
