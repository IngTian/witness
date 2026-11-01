# Codex capture

Codex conversations feed the same archive and enabled lenses as Claude Code and
OpenCode. The `default` lens is shared; `session_meta.platform = codex` and the
`codex:` session prefix preserve source attribution. A Codex distillation runner
is not required: configure the existing Claude or OpenCode runner separately.

## Import your history

Build witness, then run:

```sh
witness import --agent codex
# One native session, optionally prefixed with codex:
witness import --agent codex --session <session-id>
# Capture only, without starting a model worker:
witness import --agent codex --no-kick
```

PowerShell users can invoke `./bin/witness.exe` from a built checkout. No shell
installer or embedding model is needed for capture with `--no-kick`.

The home directory resolves as `WITNESS_CODEX_HOME`, then `CODEX_HOME`, then
`~/.codex`. Import reads:

- `thread_history_1.sqlite`, opened with SQLite `mode=ro`, for paginated history.
- `sessions/**/*.jsonl` and `archived_sessions/**/*.jsonl` for legacy rollouts.

The paginated projection takes precedence for a session that appears in both
formats. Only user and assistant text is imported. Tool calls/results, reasoning,
compaction summaries, developer/system instructions and non-text attachments are
excluded. Legacy `event_msg` copies of messages are ignored to avoid duplication.
Legacy user-role context supplied by the host may still appear in its transcript;
this adapter does not try to infer authorship from the wording of a message.

Per-session source revisions avoid rereading unchanged transcripts. Message keys
allow a growing transcript to append only its new tail; edits to earlier messages
rebuild that session's raw rows and reset its mining watermark. Import state and
raw rows commit in one transaction. A complete final JSONL record is accepted
even without a trailing newline; an incomplete live tail is deferred until the
file changes. Cleanup removes the corresponding importer state as well.

A malformed header or terminated row is isolated to its source file. Its existing
raw rows and watermark remain intact, and other sessions continue importing.
The CLI reports a skipped-input count and details go to `witness.log` (including
when `--quiet` or `--hook` suppresses human output). Destination commit failures
and cancellation remain fatal to an explicit import; hooks still log and exit 0.

These on-disk formats are version-specific, not an official stable Codex API.
An unsupported `thread_history_*.sqlite` filename, such as a future `_2`, produces
a warning and increments that same skipped-input count. It is not guessed at;
readable legacy rollouts still import. Keep the probe output when reporting a
compatibility problem.

## Keep capture current with hooks

Import is a snapshot; hooks keep it current. Run:

```sh
witness wire codex
```

Then **approve the hooks** with `/hooks` inside Codex. This step is not optional and
cannot be automated: Codex keeps non-managed hooks inert until you approve the exact
definition, so until you do, Codex runs fine and witness captures nothing.

`witness wire codex` writes three handlers into `~/.codex/hooks.json`
(`SessionStart`, `Stop`, `SessionEnd`), each invoking
`witness import --agent codex --hook`, and:

- resolves your Codex home the same way import does — `WITNESS_CODEX_HOME`, then
  `CODEX_HOME`, then `~/.codex` — so the two can never disagree about which archive is
  in play
- **merges**, preserving any hooks you wrote yourself, including other handlers on the
  same events
- is idempotent, and byte-stable: re-running it on an already-wired file writes nothing
  at all. That matters because Codex records hook trust against the hook's *hash*, so a
  pointless rewrite would revoke your approval and silently stop capture
- adopts a hand-edited entry from an earlier version of this document instead of
  appending beside it, so you will not end up importing twice per turn
- marks each handler `async`. Codex's `SessionEnd` timeout defaults to one second
  (capped at three), and reconciling a large history takes longer than that — a
  synchronous handler would be killed mid-import. It also keeps witness from ever
  blocking a Codex session.

Remove it with `witness unwire codex`, which strips only witness's own entries.

Codex runs no distillation model, so wiring it does not change your `runner`; set one
with `witness config set runner <claude|opencode>`.

Hooks receive `session_id`, which witness uses to reconcile that conversation from local
storage — rather than treating `notify` as a complete transcript feed or depending on the
hook's optional `transcript_path`. Event names, the `commandWindows` override and the
trust model are documented in the official
[Hooks guide](https://learn.chatgpt.com/docs/hooks).

### Doing it by hand

You do not need to, but the file is yours. On Windows the handler field is
`commandWindows` and the value is a PowerShell invocation — note the `&` call operator,
which a quoted path will not run without, and that an apostrophe in a path is escaped by
doubling it:

```json
{
  "hooks": {
    "Stop": [{"hooks": [{
      "type": "command",
      "commandWindows": "& 'C:\\path with spaces\\witness.exe' import --agent codex --hook",
      "async": true
    }]}]
  }
}
```

The doubled backslashes are JSON string escaping, not part of the path.

### What is verified, and what is not

Stated plainly because the failure mode here is silent. Everything on witness's side of
the boundary is tested on every OS: the document `wire codex` writes, its idempotency,
that it preserves foreign hooks, that it never claims one as its own, the Windows
`commandWindows` form and its quoting, and that the installer and importer resolve the
same Codex home. Each of those assertions was confirmed to fail when the behaviour it
checks is removed.

What is **not** verified by witness's test suite is the other side: that Codex reads
`~/.codex/hooks.json`, fires `SessionStart`/`Stop`/`SessionEnd`, and honours `async`.
That is taken from the official Hooks guide and needs a real Codex run to confirm. The
import path below *has* been exercised against real Codex data on Windows. A native
macOS run of either path has not been performed.

`--hook` always returns exit 0 and `{}` on stdout, including on malformed input,
missing history or import failure. Errors go to the witness log. The automatic
worker gate still respects `auto_distill`; no context is injected into Codex.
The existing `WITNESS_WORKER=1` recursion guard remains in effect.

Hooks may run before the local projection catches up. Repeating import is safe;
`SessionStart`, `SessionEnd` and an explicit import also recover missed work.
This is event-triggered reconciliation, not a continuously running watcher.

## Reproduce the ingest experiment

`scripts/codex-records.py` is a standard-library-only, read-only probe/converter.
Without `--session` it cannot export any conversation text:

```sh
python scripts/codex-records.py --probe
python scripts/codex-records.py --session <native-id> > private-records.ndjson
witness ingest --file private-records.ndjson
witness ingest --file private-records.ndjson
```

The second ingest should write zero records. Keep the NDJSON and test archive
private. The converter uses `file:codex:<id>` sessions because `ingest` owns the
`file:` namespace; the native adapter above uses `codex:<id>` and stamps the Codex
platform. Use a separate `WITNESS_HOME` for the experiment to avoid importing both
copies into a production archive.

## Local probe and validation

Tested on Windows with `codex-cli 0.165.0-alpha.1.2`. The inspected machine had
20 legacy JSONL files totaling 981,836,484 bytes (largest about 463 MB), 226
projected conversations, and about 14.1 GB under Codex home at probe time.
These observations require incremental import from the first release. Session
IDs are present in JSONL `session_meta` and in SQLite `thread_id`; the current
session's persisted history grows during active use, before the session ends.
CLI `--help` exposes hook trust controls; lifecycle event semantics are documented
in the official guide linked above. `config.toml` and credentials were not read.

Observed text shapes (field names only):

```text
legacy: response_item.payload = {type: message, id, role, content}
        content[] = {type: input_text | output_text, text}
current: thread_items = {thread_id, item_id, rollout_ordinal, created_at_ms,
                        item_type, item_json, ...}
         userMessage = {type, id, clientId, content: [{type: text, text, ...}]}
         agentMessage = {type, id, text, phase, memoryCitation, delivery}
cursor: thread_history_projection_state = {thread_id, next_rollout_ordinal, ...}
```

Before implementing the native adapter, a real local conversation was converted
and ingested into an isolated test archive: first run wrote 4 records, repeat wrote
0, final count 4. A separate real conversation then passed the native adapter:
first run wrote 6 records, repeat wrote 0, and every stored role/text pair exactly
matched the independent converter; platform attribution was `codex`. No model was
called, and no message content is included in this document or test fixtures.
This validates capture and idempotency; profile quality across a blended archive
still depends on the configured runner, corpus and lenses.

Validation on the working branch: `gofmt` and `go vet ./...` passed; the full
`CGO_ENABLED=0 go test ./...` suite passed on Linux under WSL. New capture and
hook tests also passed natively on Windows. The repository's complete native
Windows suite has existing failures reproduced independently on untouched upstream
commit `67b837add1756a3a44a1e3a6d6725683da6d4e40`. They include Unix-permission
assumptions, Windows file-handle cleanup/rename behavior and install asset lookup
in test binaries.

After merging v0.9.0 main (`f053ac1`), review fixes were validated with Windows
`codex-cli 0.166.0-alpha.5.1`: `CODEX_HOME` discovery imported 18 projected messages,
then discovery through the default user home imported 0 additional records into
the same isolated archive. A real 2,167,848-byte Windows rollout (writer version
`0.164.0-alpha.11.1`) imported 9 messages and then 0 on repeat. Both sources matched
the independent converter's role/text pairs exactly. At the time of probing, all
23 available rollout files ended in a newline; unterminated complete records remain
covered by synthetic crash-tail tests.

The merged branch passed Linux/WSL full tests and vet, native Windows Codex/CLI
regression tests, and macOS amd64/arm64 cross-compilation. A native macOS run has
not been performed here. All real-data checks used isolated local archives with
external runners disabled; only these aggregate results are published.
