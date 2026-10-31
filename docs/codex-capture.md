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
raw rows commit in one transaction. An incomplete final JSONL line is retried on
the next import. Cleanup removes the corresponding importer state as well.

These on-disk formats are version-specific, not an official stable Codex API.
Unsupported projections fail visibly instead of reporting an empty successful
capture. Keep the probe output when reporting a compatibility problem.

## Keep capture current with hooks

Current Codex supports lifecycle hooks; see the official
[Hooks guide](https://learn.chatgpt.com/docs/hooks). Hooks receive `session_id`,
which witness uses to reconcile the selected conversation from local storage.
This avoids treating `notify` as a complete transcript feed or depending on the
hook's optional `transcript_path`.

Merge the following entries into your user-level `~/.codex/hooks.json` (preserve
existing hooks), substituting the actual absolute executable path:

```json
{
  "hooks": {
    "SessionStart": [{"hooks": [{
      "type": "command",
      "command": "/absolute/path/to/witness import --agent codex --hook"
    }]}],
    "Stop": [{"hooks": [{
      "type": "command",
      "command": "/absolute/path/to/witness import --agent codex --hook"
    }]}],
    "SessionEnd": [{"hooks": [{
      "type": "command",
      "command": "/absolute/path/to/witness import --agent codex --hook"
    }]}]
  }
}
```

For Windows, set each handler's `commandWindows` to a PowerShell-compatible
invocation, e.g. `& 'C:\\path with spaces\\witness.exe' import --agent codex --hook`.
The example is a JSON string value, so the doubled backslashes are intentional.
Review and trust the exact hook definitions using `/hooks` in Codex; installation
alone does not grant hook trust. No command here edits Codex configuration or
changes trust settings automatically.

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
