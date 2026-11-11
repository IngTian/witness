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
readable legacy rollouts still import. Include that warning and its `witness.log`
lines when reporting a compatibility problem.

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
- marks `SessionStart` and `Stop` `async`, so capture stays off your session's critical
  path. `SessionEnd` is synchronous with a 3-second timeout — the most Codex allows for
  that event, which defaults to one second. (Codex refuses to run `SessionEnd`
  asynchronously: it runs it synchronously anyway and warns.) So on a large history
  `SessionEnd` may be cut off part-way. That is harmless: `Stop` has already reconciled
  each completed turn, import is idempotent, and anything missed is picked up by the
  next `SessionStart` or an explicit `witness import --agent codex`.

Remove it with `witness unwire codex`, which strips only witness's own entries — and
deletes `hooks.json` outright if witness created it and nothing of yours is left in it,
rather than leaving an empty file behind. A file holding any hook or key of your own is
kept.

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
    }]}],
    "SessionEnd": [{"hooks": [{
      "type": "command",
      "commandWindows": "& 'C:\\path with spaces\\witness.exe' import --agent codex --hook",
      "timeout": 3
    }]}]
  }
}
```

The doubled backslashes are JSON string escaping, not part of the path.

### What is verified

Verified against real Codex (`codex-cli 0.166.0`) on macOS, using `codex exec` into a
throwaway archive:

- **the hook path works** — a wired `Stop`/`SessionEnd` hook fires and the conversation
  reaches the archive with `codex:` attribution
- **hook trust is real and enforced.** The same run with the hooks unapproved captured
  *nothing*, silently. Approving them with `/hooks` is not boilerplate advice — it is
  the difference between working capture and an empty archive
- **import works on macOS**, both the paginated `thread_history_1.sqlite` projection and
  a legacy `rollout-*.jsonl`; a repeat import writes 0 records
- **re-wiring is inert** — the file's mtime does not move, so hook trust survives
- **unwire leaves no trace** when witness created the file

Witness's own side is additionally unit-tested on every OS — the document it writes,
idempotency, foreign-hook preservation, the Windows `commandWindows` form and its
quoting, and that the installer and importer resolve the same Codex home. Every one of
those assertions was confirmed to fail when the behaviour it checks is removed.

Not covered: the Windows *wiring* path (the import path there was verified separately),
and interactive `codex` rather than `codex exec`.

`--hook` always returns exit 0 and `{}` on stdout, including on malformed input,
missing history or import failure. Errors go to the witness log. The automatic
worker gate still respects `auto_distill`; no context is injected into Codex.
The existing `WITNESS_WORKER=1` recursion guard remains in effect.

Hooks may run before the local projection catches up. Repeating import is safe;
`SessionStart`, `SessionEnd` and an explicit import also recover missed work.
This is event-triggered reconciliation, not a continuously running watcher.
