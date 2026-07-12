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

// Run invokes `claude -p` headlessly and returns the model's text reply. systemPrompt
// is witness's own instruction (a lens extract/review prompt); input is the corpus
// being analyzed (transcript, prior observations, or facets). They are kept in
// separate turns — see buildRunCmd — so corpus text can't impersonate instructions.
// Output is the final assistant message (plain text); callers parse JSON out of it.
func (runner) Run(ctx context.Context, model, systemPrompt, input string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()

	cmd := buildRunCmd(ctx, model, systemPrompt, input)

	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("claude -p failed: %w (stderr: %s)", err, strings.TrimSpace(errb.String()))
	}
	return out.String(), nil
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
func buildRunCmd(ctx context.Context, model, systemPrompt, input string) *exec.Cmd {
	cmd := newClaudeCmd(ctx, model)
	cmd.Args = append(cmd.Args, "--append-system-prompt", systemPrompt+"\n\n"+platform.CorpusNotice)
	cmd.Stdin = strings.NewReader(platform.WrapCorpus(input))
	cmd.Env = append(os.Environ(), "WITNESS_WORKER=1")
	return cmd
}
