package claude

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/IngTian/witness/internal/platform"
	"github.com/IngTian/witness/internal/store"
)

// NewRunner mints the Claude distillation runner. Claude shells out to `claude -p`
// per Run; there is no persistent server, so Open/Close are no-ops and nothing is
// persisted to clean up (the nested run uses --no-session-persistence and the
// WITNESS_WORKER=1 recursion guard). cfg is unused today but kept in the signature
// so the RunnerProvider contract is uniform with OpenCode's model-bearing runner.
func (Platform) NewRunner(_ store.Config) platform.Runner { return runner{} }

type runner struct{}

func (runner) Open(context.Context) error { return nil }
func (runner) Close() error               { return nil }

// ValidateModels is a no-op: `claude -p` resolves models from its own environment;
// there is nothing witness can usefully pre-check.
func (runner) ValidateModels(context.Context, ...string) error { return nil }

func (runner) InvocationHint() string { return "claude -p" }

// ConcurrentRunSafe is true: each Run is a fresh, isolated `claude -p` process
// (--no-session-persistence, temp cwd, WITNESS_WORKER=1) that shares no in-process
// state, so the engine may mine many sessions at once. The only shared resource is
// the embedder, which the engine serializes on its own mutex during reduce.
func (runner) ConcurrentRunSafe() bool { return true }

// Run invokes `claude -p` headlessly and returns the model's text reply. systemPrompt
// is witness's own instruction (a lens extract/review prompt); input is the corpus
// being analyzed (transcript, prior observations, or facets). They are kept in
// separate turns — see buildRunCmd — so corpus text can't impersonate instructions.
// Output is the final assistant message (plain text); callers parse JSON out of it.
func (runner) Run(ctx context.Context, model, systemPrompt, input string) (string, error) {
	if platform.ExternalRunnersDisabled() {
		return "", fmt.Errorf("claude runner disabled by %s", platform.DisableExternalRunnersEnv)
	}
	runCtx, cancel := context.WithTimeout(ctx, claudeRunTimeout)
	defer cancel()

	cmd := buildRunCmdFn(runCtx, model, systemPrompt, input)

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
				return "", fmt.Errorf("claude -p canceled: %w (stderr: %s)", ctx.Err(), stderrExcerpt(errb.String()))
			}
			return "", fmt.Errorf("claude -p exceeded %s and was killed: %w (stderr: %s)",
				claudeRunTimeout, de, stderrExcerpt(errb.String()))
		}
		return "", fmt.Errorf("claude -p failed: %w (stderr: %s)", err, stderrExcerpt(errb.String()))
	}
	return out.String(), nil
}

// claudeRunTimeout bounds one `claude -p`. It is a var only so the deadline-attribution test can
// shrink it; nothing in production reassigns it.
var claudeRunTimeout = 10 * time.Minute

// maxStderrExcerpt bounds how much of a failed child's stderr is interpolated into the error.
//
// That error string becomes ONE line in witness.log (the worker logs it on a mine failure), and
// the child's stderr is unbounded across the 10-minute timeout — so a chatty or looping `claude`
// could write a single multi-megabyte log line. The measured real case is only ~0.5KB (repeated
// ANSI-wrapped "AWS auth refresh timed out"), so this is a ceiling on a known-possible shape
// rather than a fix for observed damage.
const maxStderrExcerpt = 4000

// stderrExcerpt trims a child's stderr and keeps its TAIL when oversized.
//
// The tail, not the head: a failing CLI puts the actual error last, after any banner or progress
// noise, so truncating from the front would preserve exactly the useless part. The marker makes
// the truncation visible so nobody debugs a silently clipped message.
func stderrExcerpt(s string) string {
	s = strings.TrimSpace(s)
	if len(s) <= maxStderrExcerpt {
		return s
	}
	return "…[" + fmt.Sprint(len(s)-maxStderrExcerpt) + " earlier bytes elided]… " + s[len(s)-maxStderrExcerpt:]
}

// newClaudeCmd builds the isolated `claude -p` invocation used for distillation:
//   - --no-session-persistence: don't write a transcript (otherwise the worker's
//     mining call appears as a stray session in whatever cwd it inherited — e.g.
//     the user's project).
//   - --strict-mcp-config: load no MCP servers. The worker needs none, and the
//     user-scope witness MCP is short-circuited by the recursion guard, so trying
//     to start it just stalls claude -p.
//   - a neutral cwd (temp dir): avoids loading the user project's CLAUDE.md/.mcp.json.
//
// model == "" omits --model so `claude -p` uses its environment default.
func newClaudeCmd(ctx context.Context, model string) *exec.Cmd {
	args := []string{"-p", "--no-session-persistence", "--strict-mcp-config"}
	if strings.TrimSpace(model) != "" {
		args = append(args, "--model", model)
	}
	cmd := exec.CommandContext(ctx, "claude", args...)
	cmd.Dir = os.TempDir()
	return cmd
}

// buildRunCmd assembles the isolated `claude -p` invocation, separating witness's
// instructions from the corpus: the instructions become the system prompt; the
// corpus is the user turn (stdin), fenced by platform.WrapCorpus so it cannot
// impersonate witness's instructions. This is the profile prompt-injection defense
// — a hostile repo that induces record_observation(<payload>) cannot have that
// payload reach the reviewer as instructions. Split out from Run so the wiring is
// unit-testable. WITNESS_WORKER=1 is the recursion guard.
// buildRunCmdFn is the test seam for the child process. It exists so the deadline-attribution
// test can drive the REAL Run() — including its error classification, the part that was wrong —
// against a stand-in that hangs, instead of spawning the user's actual `claude` CLI. Nothing in
// production reassigns it.
//
// The seam is here rather than in the test because a test that hand-builds its own exec.Cmd and
// asserts on that proves nothing about Run: an earlier test in this repo did exactly that and
// stayed green while the production path leaked unbounded children.
var buildRunCmdFn = buildRunCmd

func buildRunCmd(ctx context.Context, model, systemPrompt, input string) *exec.Cmd {
	cmd := newClaudeCmd(ctx, model)
	cmd.Args = append(cmd.Args, "--append-system-prompt", systemPrompt+"\n\n"+platform.CorpusNotice)
	cmd.Stdin = strings.NewReader(platform.WrapCorpus(input))
	cmd.Env = append(os.Environ(), "WITNESS_WORKER=1")
	return cmd
}
