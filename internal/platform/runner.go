package platform

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/IngTian/witness/internal/store"
)

const DisableExternalRunnersEnv = "WITNESS_DISABLE_EXTERNAL_RUNNERS"

func ExternalRunnersDisabled() bool { return os.Getenv(DisableExternalRunnersEnv) == "1" }

// maxStderrExcerpt bounds how much of a failed child's stderr is interpolated into the error.
//
// That error string becomes ONE line in witness.log (the worker logs it on a mine failure), and
// a child's stderr is unbounded (across claude's 20-minute timeout, say) — so a chatty or
// looping CLI could write a single multi-megabyte log line. The measured real case is only
// ~0.5KB (repeated ANSI-wrapped "AWS auth refresh timed out"), so this is a ceiling on a
// known-possible shape rather than a fix for observed damage.
const maxStderrExcerpt = 4000

// StderrExcerpt trims a child's stderr and keeps its TAIL when oversized. Every runner's
// spawned CLI goes through it.
//
// The tail, not the head: a failing CLI puts the actual error last, after any banner or progress
// noise, so truncating from the front would preserve exactly the useless part. The marker makes
// the truncation visible so nobody debugs a silently clipped message.
func StderrExcerpt(s string) string {
	s = strings.TrimSpace(s)
	if len(s) <= maxStderrExcerpt {
		return s
	}
	return "…[" + fmt.Sprint(len(s)-maxStderrExcerpt) + " earlier bytes elided]… " + s[len(s)-maxStderrExcerpt:]
}

// QuotaExhaustedError is returned when a shared usage limit is reached and will reset
// at a known time. cmd's gate pauses that runtime's SharedAllowance calls until ResetAt;
// the drain skips the lens without a backoff.
type QuotaExhaustedError struct {
	ResetAt time.Time
	Runtime string
}

func (e *QuotaExhaustedError) Error() string {
	return fmt.Sprintf("%s usage limit reached; resets at %s", e.Runtime, e.ResetAt.UTC().Format(time.RFC3339))
}

// SharedAllowance: this call draws on an allowance one QuotaExhaustedError refusal exhausts for every
// such call on this runtime until its reset (OpenCode Zen's free models, counted per network; a Codex
// plan login; a Claude login, armed only by the CLI's rejected rate_limit_event). cmd pauses exactly
// those calls, per runtime. NewRunner must stay side-effect free:
// cmd mints runners without opening them to ask this.
type SharedAllowance interface{ OnSharedAllowance(model string) bool }

// RunnerLoginGate lets wire leave capture usable until the runner has a login.
type RunnerLoginGate interface {
	ReadyToBind(ctx context.Context, root string) error
}

// ErrRunnerBusy means a diagnostic could not inspect a runner during an active call.
var ErrRunnerBusy = errors.New("runner busy: another process holds the login lock")

type RunnerAuthenticator interface {
	Login(ctx context.Context, root string) error
	Logout(ctx context.Context, root string) error
}

// ModelFormat lets wire ask in the runtime's own syntax. Runtimes without it
// retain OpenCode's provider/model prompt for compatibility.
type ModelFormat interface {
	ModelFormat() (placeholder, description string)
	ValidModel(model string) bool
}

// Runner is the default distillation-engine lifecycle: one engine drains every
// pending session regardless of which platform produced it. It is the whole of
// what the engine (internal/distill) knows about a runtime — distill calls Run and
// never learns whether that shells to `claude -p` or talks to `opencode serve`.
// This is the axis resolved by RunnerForName (by cfg.Runner), the counterpart to
// ForSession (the per-session owning platform).
type Runner interface {
	// Open acquires whatever the engine needs to serve Run (OpenCode: a private
	// isolated `opencode serve`; Claude: nothing). Callers may skip Open when there is
	// no work; Close must tolerate an unopened runner.
	Open(ctx context.Context) error
	// Run performs one mining/review/summarize pass. systemPrompt is witness's own
	// instruction; input is the corpus being analyzed. Both arrive ALREADY FENCED:
	// distill.RunnerMine, the seam every production Run goes through, appends CorpusNotice to
	// systemPrompt and wraps input with WrapCorpus. A runner must pass them through verbatim,
	// systemPrompt as the system/instruction channel and input as the user turn, and must not
	// fence again.
	//
	// Two more rules every runner must follow, because nothing else enforces them:
	//
	//   - Its OWN timeout must surface as an error that errors.Is(err, context.DeadlineExceeded).
	//     distill reads that as provider backpressure and narrows the drain window. os/exec does
	//     not do it for you: a CommandContext kill returns "signal: killed", which is NOT
	//     DeadlineExceeded. That silently disabled backpressure on the Claude path once; check
	//     the run context's Err() and wrap it (see the claude runner).
	//   - It must refuse to spawn anything when ExternalRunnersDisabled() is true (`make test`
	//     sets it), so a test can never start a real model CLI.
	//
	// FRESH-CONTEXT INVARIANT. The only turns witness puts into a Run are systemPrompt and the
	// fenced corpus; a runtime may add its own framing (listed per runner below). A runner must
	// NEVER inherit a conversation witness did not author. A Run must offer the model no tools,
	// because the corpus is untrusted and a tool turns an injected instruction into an action.
	//
	// This is a correctness requirement, not hygiene, and it is stated here because it was
	// previously only implied — each adapter restated it in prose, nothing defined or enforced
	// it, and one path silently drifted. Three things depend on it:
	//
	//   - The FENCE means what it says. WrapCorpus + CorpusNotice tell the model the user turn
	//     is untrusted data. Inheriting that same material as the model's own context tells it
	//     the opposite, in the same request.
	//   - The engine calls Run concurrently across sessions, which is safe because each call
	//     gets an isolated session. Seeding a context from somewhere else makes that false.
	//   - The reply must be FINDABLE. A runner that collects its answer by scanning a
	//     conversation for its own request needs that request near the start; inherited history
	//     pushed it out of the window and the poll then spun to the timeout — the real cause of
	//     `context deadline exceeded` on OpenCode, long misread as a slow model.
	//
	// Claude isolates: no prior or saved conversation (one-shot `-p --no-session-persistence`); no
	// built-in tools (`--tools ""`); no MCP servers (`--strict-mcp-config`); not the user's
	// project CLAUDE.md, .mcp.json or .claude/ settings (the cwd is witness's own empty
	// <RuntimeRoot>/claude-cwd, shared by every Run, never under the world-writable temp dir); and,
	// through `--safe-mode` (2.1.169+), no user, ancestor or managed CLAUDE.md, auto memory, plugin,
	// skill, output style or user/project/plugin hook.
	//
	// Claude does NOT isolate: Claude Code's own system prompt (witness's is appended via
	// `--append-system-prompt`) and admin-managed policy settings and their hooks. `--safe-mode`, not
	// `--bare`: bare limits Anthropic auth to ANTHROPIC_API_KEY/apiKeyHelper, dropping every claude.ai
	// OAuth login.
	//
	// Codex: a fresh ephemeral exec with a witness-owned home, an empty cwd and no user
	// or project instructions. The CLI's policy preflight rejects managed layers.
	// Feature flags and a stripped model catalog remove tool offers. Server-side
	// framing under a ChatGPT login remains outside the runner's control.
	//
	// OpenCode: a fresh session in the private DB, prompted once and deleted when Run returns.
	// Witness's prompt is the agent's system prompt. Private config, nothing from the user's
	// opencode.json; `OPENCODE_DISABLE_PROJECT_CONFIG=1`, `OPENCODE_DISABLE_CLAUDE_CODE=1`.
	// `denyAll` on the agent AND the session. Not isolated (machine-wide admin config, skill
	// discovery): docs/opencode.md "The isolated runtime".
	//
	// Enforced by: `TestNoCodePathForksAConversation` (source scan, OpenCode no-fork);
	// `TestContractToolFreeWire` (tag `codexcontract`, Codex wire contract);
	// `TestContractRunnerRoundTrip` (tag `opencodecontract`, contract workflow: zero tools on real
	// OpenCode); `TestBuildRunCmdRoleSeparation` and `TestBuildRunCmdOffersNoTools` pin the Claude
	// argv, checked once on real claude 2.1.327 on Bedrock (2026-11-18, not in CI). With
	// `--safe-mode`, the stream-json init showed `tools: []`, `mcp_servers: []`, no plugin or user
	// skill and 0 hook events, the result had `is_error:false`, and the appended probe marker came
	// back. The control without the flag showed 12 plugins, 39 skills, 20 hook events,
	// saw_user_profile:true.
	Run(ctx context.Context, model, systemPrompt, input string) (string, error)
	// Close releases engine resources.
	Close() error
	// ValidateModels reports whether the configured models are usable by this engine
	// (feeds `witness doctor`). Claude is a no-op; OpenCode checks its model list.
	ValidateModels(ctx context.Context, models ...string) error
	// InvocationHint is a short human string naming how this engine runs, for
	// doctor/diagnostics (e.g. "claude -p" / "opencode serve --stdio").
	InvocationHint() string
}

// (ConcurrentRunSafe used to live here: "may the engine call Run concurrently?" Every runner has
// answered true since #22 narrowed OpenCode's mutex, so the drain always runs its window wide. A
// runner that ever needs serializing can hold its own lock inside Run.)

// RunnerProvider is the Platform capability that builds this platform's Runner.
// Kept separate from Runner so a Platform value (a stateless registry entry) mints
// a fresh, cfg-bound Runner per drain rather than being one itself.
type RunnerProvider interface {
	NewRunner(cfg store.Config) Runner
}

// (SweepsOnCloser used to live here: "does Close sweep shared state a live worker needs?", so
// `witness lens try` could take WorkerLock only then. True in #72, when OpenCode's Close swept the
// SHARED database; v0.7.0's private database deleted that sweep, and the Open path now sweeps only
// week-old OpenCode serve dirs. A sweeping runner would need three lines of locking at the call site.)

// RunnerForName mints the Runner for an explicit runner NAME: empty means the default
// platform, and an unknown name fails closed so a typo surfaces instead of silently
// defaulting. Callers resolve the name first — the store's precedence ladder
// (store.ResolveRunner) for the default, then distill.RunnerFor for a lens's own runtime —
// and mint by name. There is deliberately no store-taking variant: re-applying the ladder
// after a lens has picked its runtime let an unbound store's WITNESS_RUNNER override the
// lens, so `lens try` previewed on a different runtime than the drain used (F030).
//
// This is independent of ForSession: a Claude-runner user distilling imported OpenCode
// sessions mints Claude (shells to claude -p) while each session's ForSession=OpenCode still
// shapes its input. cfg is passed through for the runner's model prewarm/validate.
func RunnerForName(name string, cfg store.Config) (Runner, error) {
	name = strings.TrimSpace(name)
	// An empty runner means "unset" — fall back to the default platform (Claude),
	// matching the config default and the old NewRunner's `case "", "claude"`. A
	// NON-empty but unrecognized name still fails closed (a real typo).
	var p Platform
	var ok bool
	if name == "" {
		p = Default()
	} else if p, ok = ByName(name); !ok {
		return nil, unknownRunnerError(name)
	}
	rp, ok := p.(RunnerProvider)
	if !ok {
		return nil, unknownRunnerError(name)
	}
	return rp.NewRunner(cfg), nil
}
