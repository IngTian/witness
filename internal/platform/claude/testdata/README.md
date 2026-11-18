# Claude runner stream fixtures

- `cloud-credential-error.stream.jsonl` is a **real** capture from Claude Code 2.1.327 (`claude -p --safe-mode --no-session-persistence --strict-mcp-config --tools "" --output-format stream-json --verbose`). The run failed on cloud credentials: managed settings forced Bedrock, and no AWS credentials were present. Its cwd is scrubbed. The run made no model call. It **must not** pause: `is_error:true`, `subtype:"success"`, `terminal_reason:"api_error"`, and no `rate_limit_event`.
- `usage-limit.stream.jsonl` is **hand-built**. A subscription usage limit could not be captured from the real CLI on the maintainer's machine, because managed settings force Bedrock there.
  - The init, assistant and result shapes come from the real 2.1.327 capture above.
  - The `rate_limit_event` (`status:"rejected"`, `resetsAt` in epoch seconds) and the result's `api_error_status:429` / `api_error:"usage_limit_reached"` follow the format reported in anthropics/claude-code#103820 and #103807 (2.1.304).
  - `resetsAt` is 1795000000. Tests rewrite it relative to their own clock.
- `success.stream.jsonl` is a **real** capture from the Bedrock proof call with `--safe-mode`, scrubbed (see `internal/platform/runner.go`'s "Enforced by" line).
