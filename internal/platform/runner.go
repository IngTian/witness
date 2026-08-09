package platform

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"

	"github.com/IngTian/witness/internal/store"
)

const DisableExternalRunnersEnv = "WITNESS_DISABLE_EXTERNAL_RUNNERS"

func ExternalRunnersDisabled() bool { return os.Getenv(DisableExternalRunnersEnv) == "1" }

// NativeSession identifies one L0 input whose owning runtime keeps its own conversation
// store. Runners that support native isolation use it to RETAIN an isolated scratch context
// until that input's L1 commit succeeds, so a crash mid-generation resumes instead of
// re-billing the model. Other runners ignore it. The finalizer is set by the runner and
// called only by distill after the generation CAS succeeds.
//
// "Scratch context", deliberately, not "fork": the retained thing must be seeded only by
// witness's own turns (see the fresh-context invariant on Runner.Run). This port named it a
// fork after OpenCode's /session/{id}/fork endpoint, which put one runtime's mechanism in
// the cross-runtime vocabulary — and the fork's inherited history was itself the bug.
type NativeSession struct {
	Session  string
	RawHigh  int64
	Total    int
	Lens     string
	Input    string
	Digest   string
	mu       sync.Mutex
	finalize func() error
}

type TranscriptEntry struct {
	Role string `json:"role"`
	Text string `json:"text"`
}

func TranscriptDigest(entries []TranscriptEntry) string {
	b, _ := json.Marshal(entries)
	return fmt.Sprintf("%x", sha256.Sum256(b))
}

// NativeSessionSupport is implemented only by runners that understand the retained
// scratch-context protocol (create once, prompt, resume on retry, delete after L1 is durable).
type NativeSessionSupport interface{ SupportsNativeSession() bool }

// NativeSessionSource is the PLATFORM-side half of the same capability: a platform whose sessions
// live in its own conversation store, so a runner can retain a scratch context keyed to one of
// them and resume it after a crash.
//
// Both halves are needed, and until now only one was a capability. The runner half was already
// asked politely (runnerSet.NativeFor asserts NativeSessionSupport); the SESSION half was a
// hardcoded string compare in the engine — `ForSession(...).Name() == AgentOpenCode` in
// internal/distill — which is exactly the platform-name dispatch the architecture forbids and
// internal/platform/acceptance_test.go exists to catch. That guard missed it: none of its patterns
// match a `.Name() ==` equality, so the single real violation sat inside the fence it was written
// to enforce. Asking here instead means a third runtime with its own session store implements a
// method rather than editing the engine.
//
// Nil-safe access is SupportsNativeSessionSource below.
type NativeSessionSource interface{ SupportsNativeSessions() bool }

// SupportsNativeSessionSource reports whether p's sessions can host a retained scratch context —
// nil-safe and assertion-safe, so the engine never has to know which platforms qualify. A platform
// that does not implement the capability answers false, which is the correct default: without a
// private conversation store there is nothing to retain.
func SupportsNativeSessionSource(p Platform) bool {
	if p == nil {
		return false
	}
	src, ok := p.(NativeSessionSource)
	return ok && src.SupportsNativeSessions()
}

type nativeSessionKey struct{}

func WithNativeSession(ctx context.Context, n *NativeSession) context.Context {
	return context.WithValue(ctx, nativeSessionKey{}, n)
}

func NativeSessionFromContext(ctx context.Context) *NativeSession {
	n, _ := ctx.Value(nativeSessionKey{}).(*NativeSession)
	return n
}

func (n *NativeSession) SetFinalizer(f func() error) { n.mu.Lock(); n.finalize = f; n.mu.Unlock() }
func (n *NativeSession) Finalize() error {
	n.mu.Lock()
	f := n.finalize
	n.mu.Unlock()
	if f == nil {
		return nil
	}
	return f()
}

// Runner is the default distillation-engine lifecycle: one engine drains every
// pending session regardless of which platform produced it. It is the whole of
// what the engine (internal/distill) knows about a runtime — distill calls Run and
// never learns whether that shells to `claude -p` or talks to `opencode serve`.
// This is the axis resolved by RunnerFor (by cfg.Runner), the counterpart to
// ForSession (the per-session owning platform).
type Runner interface {
	// Open acquires whatever the engine needs to serve Run (OpenCode: a private
	// isolated `opencode serve`; Claude: nothing). Callers may skip Open when there is
	// no work; Close must tolerate an unopened runner.
	Open(ctx context.Context) error
	// Run performs one mining/review/summarize pass. systemPrompt is witness's own
	// instruction; input is the corpus being analyzed — the platform fences it with
	// WrapCorpus so it cannot impersonate instructions.
	//
	// FRESH-CONTEXT INVARIANT. The context a Run executes in must be seeded EXCLUSIVELY by
	// witness's own turns: systemPrompt plus the fenced corpus, and nothing else. A runner may
	// retain such a context across a crash to resume one generation, and may add its own retry
	// turns to it, but must NEVER inherit a conversation witness did not author.
	//
	// This is a correctness requirement, not hygiene, and it is stated here because it was
	// previously only implied — each adapter restated it in prose, nothing defined or enforced
	// it, and one path silently drifted. Three things depend on it:
	//
	//   - The FENCE means what it says. WrapCorpus + CorpusNotice tell the model the user turn
	//     is untrusted data. Inheriting that same material as the model's own context tells it
	//     the opposite, in the same request.
	//   - ConcurrentRunSafe (below) is justified by "an isolated session per call". Seeding a
	//     context from somewhere else makes that justification false.
	//   - The reply must be FINDABLE. A runner that collects its answer by scanning a
	//     conversation for its own request needs that request near the start; inherited history
	//     pushed it out of the window and the poll then spun to the timeout — the real cause of
	//     `context deadline exceeded` on OpenCode, long misread as a slow model.
	//
	// Both runners satisfy it: Claude via `claude -p --no-session-persistence` (a stateless
	// one-shot process), OpenCode by creating a fresh session in its private database, prompting
	// it once, and deleting it after L1 is durable. internal/platform/opencode enforces the
	// OpenCode half with a source scan (TestNoCodePathForksAConversation).
	Run(ctx context.Context, model, systemPrompt, input string) (string, error)
	// Close releases engine resources.
	Close() error
	// ValidateModels reports whether the configured models are usable by this engine
	// (feeds `witness doctor`). Claude is a no-op; OpenCode checks its model list.
	ValidateModels(ctx context.Context, models ...string) error
	// InvocationHint is a short human string naming how this engine runs, for
	// doctor/diagnostics (e.g. "claude -p" / "opencode serve").
	InvocationHint() string
	// ConcurrentRunSafe reports whether it is safe for the engine to call Run
	// concurrently (several sessions mining at once) against this runner. This is a
	// platform FACT (mechanism), NOT a policy: the engine owns the pool size and the
	// ceiling; the platform only states whether overlap is safe at all. Both runtimes
	// are true today: Claude — each Run is an independent `claude -p` process sharing
	// nothing; OpenCode — Run holds its mutex only for a closed-check and drives an
	// isolated session per call over the shared serve process, which a benchmark showed
	// accepts many concurrent sessions (issue #22 narrowed the mutex to flip this true).
	// "Isolated session per call" is load-bearing here, and is exactly the fresh-context
	// invariant on Run: while native mining forked the user's conversation this claim was
	// not true in its own stated terms, since the per-call session was not isolated from
	// that conversation.
	ConcurrentRunSafe() bool
}

// RunnerProvider is the Platform capability that builds this platform's Runner.
// Kept separate from Runner so a Platform value (a stateless registry entry) mints
// a fresh, cfg-bound Runner per drain rather than being one itself.
type RunnerProvider interface {
	NewRunner(cfg store.Config) Runner
}

// (SweepsOnCloser / RunnerSweepsOnClose used to live here.)
//
// The capability asked "does closing this runner run a PROCESS-GLOBAL sweep that could disturb a
// concurrently-running worker?", so that a read-only tool (`witness lens try`) could take the
// machine-wide WorkerLock only when the answer was yes. It was TRUE when added in #72: OpenCode's
// Close ran cleanupDistillSessions, a sweep of the SHARED OpenCode database that deleted
// witness-distill sessions created before now+1s — which would delete a live worker's in-flight
// session.
//
// Both halves of that premise are gone. v0.7.0 gave distillation its own private database, so
// cleanupDistillSessions was deleted outright (the symbol no longer exists anywhere). The sweeps that
// remain on the OpenCode Open path — reapPriorServe and procCtl.ReapOrphans — are ORPHAN-ONLY by
// construction: reapPriorServe's first gate requires the owning witness process to be GONE, and
// ReapOrphans gates on ppid==1, so neither can reach a live worker's state. That is why the only
// implementation returned false, and it was CORRECT rather than stale.
//
// So the interface had exactly one implementation, hardcoded to the value that makes its guarded
// branch unreachable. Deleted rather than kept "in case a future runner sweeps": a capability whose
// single answer is no is not a seam, it is a comment. If a sweeping runner ever appears, the lock it
// needs is three lines at the call site — and this note records what shape the problem took.

// RunnerFor resolves the default runner for a drain. It applies the store's runner
// precedence (bound-meta > config line > WITNESS_RUNNER env > default — unchanged)
// to get ONE name, then mints that platform's Runner. Fails closed on an unknown
// name so a typo surfaces instead of silently defaulting.
//
// This is deliberately independent of ForSession: a Claude-runner user distilling
// imported OpenCode sessions resolves RunnerFor=Claude (shells to claude -p) while
// each session's ForSession=OpenCode still shapes its input. One engine, per-source
// input shaping — the two axes never derive from each other.
func RunnerFor(st store.RunnerResolver, cfg store.Config) (Runner, error) {
	return RunnerForName(strings.TrimSpace(st.ResolveRunner(cfg)), cfg)
}

// RunnerForName mints the Runner for an explicit runner NAME, applying the same
// empty→default + fail-closed-on-typo rules as RunnerFor. It is the per-lens-runner seam
// (issue #75 slice 2): the worker resolves each active lens's runtime (distill.RunnerFor)
// and mints one runner per distinct name via this, so lenses on different runtimes each
// get their own Runner. cfg is passed through for the runner's model prewarm/validate; a
// caller with per-lens models on this runtime should pass a cfg carrying the union (see
// the worker's per-runtime open).
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
