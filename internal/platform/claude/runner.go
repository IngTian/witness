package claude

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/IngTian/witness/internal/platform"
	"github.com/IngTian/witness/internal/store"
)

// NewRunner mints the Claude distillation runner. Claude shells out to `claude -p`
// per Run; there is no persistent server, so Open/Close are no-ops (the nested run uses
// --no-session-persistence and the WITNESS_WORKER=1 recursion guard). cfg supplies only
// RuntimeRoot, under which every Run's cwd lives (see workDir).
func (Platform) NewRunner(cfg store.Config) platform.Runner {
	return runner{runtimeRoot: cfg.RuntimeRoot}
}

type runner struct{ runtimeRoot string }

func (runner) Open(context.Context) error { return nil }
func (runner) Close() error               { return nil }

// ValidateModels is a no-op: `claude -p` resolves models from its own environment;
// there is nothing witness can usefully pre-check.
func (runner) ValidateModels(context.Context, ...string) error { return nil }

func (runner) InvocationHint() string { return "claude -p" }

// Run invokes `claude -p` headlessly and returns the model's text reply. systemPrompt
// is witness's own instruction (a lens extract/review prompt); input is the corpus
// being analyzed (transcript, prior observations, or facets). They are kept in
// separate turns — see buildRunCmd — so corpus text can't impersonate instructions.
// Output is the final assistant message (plain text); callers parse JSON out of it.
func (r runner) Run(ctx context.Context, model, systemPrompt, input string) (string, error) {
	if platform.ExternalRunnersDisabled() {
		return "", fmt.Errorf("claude runner disabled by %s", platform.DisableExternalRunnersEnv)
	}
	dir, err := r.workDir()
	if err != nil {
		return "", err
	}
	runCtx, cancel := context.WithTimeout(ctx, claudeRunTimeout)
	defer cancel()

	cmd := buildRunCmdFn(runCtx, model, systemPrompt, input)
	cmd.Dir = dir

	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil {
		// A TIMEOUT must be reported as context.DeadlineExceeded, and os/exec does not do that
		// for us. When CommandContext kills the child, Run returns *exec.ExitError "signal:
		// killed" — errors.Is(err, context.DeadlineExceeded) is FALSE (verified directly). So
		// wrapping the raw error alone made the engine's backpressure signal permanently dead on
		// the Claude path: distill classifies a deadline as MineTimedOut and narrows the drain
		// window on it, and a `claude -p` that ran out of time was instead filed as a generic
		// transport failure that only backs the lens off. Concurrency never narrowed, so the
		// starvation this branch fixes for OpenCode would have kept recurring here.
		//
		// runCtx.Err() is the discriminator rather than inspecting the exit status: it says
		// whether WE stopped the child, which "signal: killed" cannot distinguish from the user
		// killing it or an OOM.
		if de := runCtx.Err(); de != nil {
			// Report the CALLER's cancellation as cancellation, not as our timeout: if the parent
			// ctx is already done the worker is shutting down, which is not a provider signal and
			// must not narrow the window.
			if ctx.Err() != nil {
				return "", fmt.Errorf("claude -p canceled: %w (stderr: %s)", ctx.Err(), childOutput(errb.String(), out.String()))
			}
			return "", fmt.Errorf("claude -p exceeded %s and was killed: %w (stderr: %s)",
				claudeRunTimeout, de, childOutput(errb.String(), out.String()))
		}
		return "", fmt.Errorf("claude -p failed: %w (stderr: %s)", err, childOutput(errb.String(), out.String()))
	}
	return out.String(), nil
}

// workDir is the one cwd every Run shares: <RuntimeRoot>/claude-cwd, 0700, never removed.
//
// Claude Code reads .claude/settings.json (hooks) and .mcp.json from its cwd, and CLAUDE.md,
// .claude/CLAUDE.md, .claude/rules/*.md and CLAUDE.local.md (AGENTS.md as a fallback) from the cwd
// AND every ancestor, with no ownership check. So the cwd must be witness-owned and empty, and no
// ancestor may be writable by another user. Anything under os.TempDir() fails that on Linux: a
// planted /tmp/CLAUDE.md steers every distillation even from a private dir below it. RuntimeRoot
// sits under the user's data dir, so its ancestors are the user's own dirs.
//
// Stable, not per-Run: Claude Code makes a ~/.claude/projects/<cwd>/memory/ dir for each new cwd,
// so a fresh dir per call left one behind per model call (thousands for a backfill). Concurrent
// Runs share it safely: with --tools "" nothing writes there.
func (r runner) workDir() (string, error) {
	// Fail closed: an empty root would make the cwd relative to wherever the worker started,
	// which may be the very project dir this avoids.
	if r.runtimeRoot == "" {
		return "", errors.New("claude runner: witness runtime root is unavailable")
	}
	dir := filepath.Join(r.runtimeRoot, "claude-cwd")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("claude runner: create cwd: %w", err)
	}
	// MkdirAll keeps an existing dir's mode; tighten one created looser. On Windows Chmod only
	// toggles read-only, and 0o700 keeps the dir writable, so this is harmless there.
	if err := os.Chmod(dir, 0o700); err != nil {
		return "", fmt.Errorf("claude runner: restrict cwd: %w", err)
	}
	return dir, nil
}

// claudeRunTimeout bounds one `claude -p`. It is a var only so the deadline-attribution test can
// shrink it; nothing in production reassigns it.
//
// 20 minutes, raised from 10 by the owner's call: compaction's union writes need it (generation
// time tracks output, and the owner's largest duplicate cluster writes ~26k chars where ~21k was
// the 10-minute ceiling), and the worker is async, so a wedged call holding it for up to 20
// minutes is the accepted price.
var claudeRunTimeout = 20 * time.Minute

// childOutput is a failed child's stderr excerpt plus, when there is any, a bounded tail of its
// stdout — which is where `claude -p` prints its own errors. Reporting stderr alone logged the
// default-lens stall 322 times as `exit status 1 (stderr: )`.
func childOutput(stderr, stdout string) string {
	s := platform.StderrExcerpt(stderr)
	if o := platform.StderrExcerpt(stdout); o != "" {
		s += "; stdout: " + o
	}
	return s
}

// newClaudeCmd builds the isolated `claude -p` invocation used for distillation:
//   - --no-session-persistence: don't write a transcript (otherwise the worker's
//     mining call appears as a stray session in whatever cwd it inherited — e.g.
//     the user's project).
//   - --strict-mcp-config: load no MCP servers. The worker needs none, and the
//     user-scope witness MCP is short-circuited by the recursion guard, so trying
//     to start it just stalls claude -p.
//   - --tools "": offer no built-in tool (MCP already off). The corpus is untrusted
//     and a tool turns an injected instruction into an action. The 2026-11-16 audit
//     (D1) caught the model using tools; a tool that names no tools is still a tool
//     (the model saw ["web_search"]). A CLI too old for the flag exits non-zero and
//     Run reports its stdout and stderr (childOutput), so no doctor probe.
//   - Run sets the cwd to witness's own empty <RuntimeRoot>/claude-cwd (workDir), never
//     the user's project or os.TempDir(), so no foreign CLAUDE.md, .mcp.json or
//     .claude/settings.json hook loads.
//
// model == "" omits --model so `claude -p` uses its environment default.
func newClaudeCmd(ctx context.Context, model string) *exec.Cmd {
	args := []string{"-p", "--no-session-persistence", "--strict-mcp-config", "--tools", ""}
	if strings.TrimSpace(model) != "" {
		args = append(args, "--model", model)
	}
	cmd := exec.CommandContext(ctx, "claude", args...)
	return cmd
}

// buildRunCmdFn is the test seam for the child process. It exists so the deadline-attribution
// test can drive the REAL Run() — including its error classification, the part that was wrong —
// against a stand-in that hangs, instead of spawning the user's actual `claude` CLI. Nothing in
// production reassigns it.
//
// The seam is here rather than in the test because a test that hand-builds its own exec.Cmd and
// asserts on that proves nothing about Run: an earlier test in this repo did exactly that and
// stayed green while the production path leaked unbounded children.
var buildRunCmdFn = buildRunCmd

// buildRunCmd assembles the isolated `claude -p` invocation, separating witness's
// instructions from the corpus: the instructions become the system prompt; the
// corpus is the user turn (stdin). Both arrive already fenced (distill.RunnerMine applies
// platform.WrapCorpus + CorpusNotice for every runner), so the corpus cannot impersonate
// witness's instructions — a hostile repo that induces record_observation(<payload>)
// cannot have that payload reach the reviewer as instructions. Split out from Run so the
// wiring is unit-testable. WITNESS_WORKER=1 is the recursion guard.
func buildRunCmd(ctx context.Context, model, systemPrompt, input string) *exec.Cmd {
	cmd := newClaudeCmd(ctx, model)
	cmd.Args = append(cmd.Args, "--append-system-prompt", systemPrompt)
	cmd.Stdin = strings.NewReader(input)
	cmd.Env = append(os.Environ(), "WITNESS_WORKER=1")
	return cmd
}
