package opencode

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/IngTian/witness/internal/proc"
)

// procCtl is the process-control port (issue #43): openCodeCommand binds every opencode child's
// lifetime to the worker through it (Pdeathsig on Linux). A package var so tests can swap in a
// proc.Fake to prove the wiring without spawning real processes.
var procCtl proc.Control = proc.System()

var openCodeVersionCommand = func(ctx context.Context, runtimeRoot string) ([]byte, error) {
	return openCodeCommand(ctx, runtimeRoot, "--version").Output()
}

var openCodeAsyncPollInterval = time.Second

// OpenCodeServer is the HTTP client for one private `opencode serve` (serve.go). Each Run creates a
// fresh short-lived OpenCode session, sends one prompt, then deletes that session so distillation
// calls never share conversation context.
type OpenCodeServer struct {
	serve      *privateServe // nil for a server built over a test endpoint
	baseURL    string
	authHeader string
	configPath string
	client     *http.Client
	sem        chan struct{} // one slot per parallel generation (maxConcurrentGenerations)

	agentMu sync.Mutex
	agents  map[string]string // agentID -> system prompt: every agent written into configPath
	loaded  sync.Map          // agentID -> error: nil once the serve listed it, else its load failure

	mu     sync.Mutex
	closed bool
}

// maxConcurrentGenerations caps the generations one serve runs at once. A var read at construction,
// so a test (and PR 3's 16-way check) sets it before building the server.
var maxConcurrentGenerations = 8

// newOpenCodeServer is the one constructor; StartOpenCodeServerIn attaches the serve it owns.
func newOpenCodeServer(baseURL, authHeader, configPath string, client *http.Client) *OpenCodeServer {
	return &OpenCodeServer{baseURL: baseURL, authHeader: authHeader, configPath: configPath, client: client,
		sem: make(chan struct{}, maxConcurrentGenerations), agents: map[string]string{}}
}

// StartOpenCodeServerIn starts a private lease serve under runtimeRoot and returns its client.
// It validates the version, refuses an empty model, spawns the serve, and validates that the
// configured models are available. On any failure it closes the serve.
//
// There is no runtimeRoot-less variant: the empty root once meant "use the AMBIENT OpenCode
// database", i.e. opt out of the private per-witness database that #119 made the isolation
// boundary, so callers pass the root explicitly and an empty one is refused, before any spawn.
func StartOpenCodeServerIn(ctx context.Context, runtimeRoot string, models []string) (*OpenCodeServer, error) {
	if runtimeRoot == "" {
		return nil, errors.New("opencode isolated runtime root is unavailable")
	}
	if err := requireModels(models); err != nil {
		return nil, err
	}
	if err := ValidateOpenCodeCapability(ctx); err != nil {
		return nil, err
	}
	serve, err := startPrivateServe(ctx, runtimeRoot, allowsFree(models))
	if err != nil {
		return nil, err
	}
	srv := newOpenCodeServer(serve.URL, serve.AuthHeader(), serve.ConfigPath(), &http.Client{})
	srv.serve = serve
	if err := srv.validateModels(ctx, models); err != nil {
		_ = srv.Close()
		return nil, err
	}
	return srv, nil
}

// generateTimeout bounds ONE generation once it has started (#143: queue time has its own
// queueWaitBudget). Owner's decision 2026-11-13: 20 minutes, the same wall as claudeRunTimeout
// ("it's async"). Compaction's union cap (distill.compactUnionMaxChars, 60k chars) is sized for a
// 20-minute write; at 10 a large merge on a slow free model timed out on every attempt and resent
// up to 60k characters to the shared free allowance each time. The price: a stuck generation holds
// the machine-wide WorkerLock up to 20 minutes before its lens backs off. A var so a test can
// shrink it to prove the phase is enforced.
var generateTimeout = 20 * time.Minute

// queueWaitBudget bounds how long a request may sit UNSTARTED at the provider before witness
// gives up on it. "Unstarted" includes OpenCode's own retry backoff (an assistant record whose
// `retry` is set; ~85 s for its 10 short retries, runner.md (c)(3)). It is deliberately generous —
// longer than generateTimeout — because a provider that serves serially makes concurrent siblings
// wait by design, and killing them for that is precisely the bug this exists to prevent: with
// mine_concurrency=4 and ~41s per generation, the last sibling legitimately waits ~2 minutes
// before it starts.
//
// It is not unbounded, because a request that never starts must eventually surface rather than
// pin a drain forever. The engine's own backoff then retries the session, and the drain's
// adaptive narrowing reduces how many siblings compete next time. A retry the provider scheduled
// past what is left of this budget gives up at once (waitForReply) rather than waiting it out.
// A var, not a const, for the same reason openCodeRequestTimeout and healthCheckBudget are: a test
// must be able to shrink it to prove the budget is ENFORCED. Asserting on a 30-minute bound with a
// real clock is not a test anyone will run.
var queueWaitBudget = 30 * time.Minute

// requestLifetimeBudget is the ABSOLUTE ceiling a single Run may occupy: the queue allowance plus
// one generation, with slack so the phase checks above (which produce actionable messages naming
// WHICH phase failed) always fire before this blunt ctx deadline does.
//
// The ctx bound must cover BOTH phases. It previously used generateTimeout alone, which meant a
// request that legitimately waited its turn at a serially-serving provider was killed by the ctx
// at 600s no matter what the phase logic decided — the very starvation the two-phase budget
// exists to remove. It stays finite so a wedged serve can never pin the machine-wide WorkerLock
// indefinitely.
//
// WHAT THIS COSTS AT THE DRAIN LEVEL, since the number is much larger than it looks: 30m + 20m +
// 1m = 51m is the worst case for ONE Run, and a session with N enabled lenses issues N sequential
// Runs, so a single wholly-unserved session can hold the WorkerLock for N*51m. At 3 lenses that is
// ~2.5h. Nothing bounds a drain's total wall clock; only the per-request ceiling is bounded.
//
// That is deliberate but it is a real ceiling worth knowing before raising queueWaitBudget:
//   - It is only reachable when the provider accepts requests and serves NONE of them. A provider
//     that serves anything moves the request into the generation phase, where the bound is 20m.
//   - Backoff is what stops the repeat. A lens whose mine deadlines is backed off (MineFailed), so
//     the pathological case does not re-run immediately on the next drain.
//   - The lock is held, not leaked: witness stays responsive, `witness distill status` reports the
//     worker as running, and the drain's ctx cancellation still cuts every in-flight Run.
//
// The alternative — a drain-wide deadline — was not added because it would abandon a legitimately
// queued backfill partway through with no way to distinguish it from starvation, and the queue is
// already resumable by design (the watermark makes the next drain pick up exactly where this one
// stopped).
func requestLifetimeBudget() time.Duration { return queueWaitBudget + generateTimeout + time.Minute }

// openCodeRequestTimeout bounds ONE HTTP request to the local serve process, and
// openCodeProbeTimeout bounds ONE `opencode` CLI probe (`--version`, the only one left).
//
// Both are liveness backstops for the same hazard generateTimeout guards: the drain's ctx
// is signal-cancellable but has no deadline, the shared http.Client has no Timeout, and
// exec.CommandContext only kills on ctx cancellation. So a wedged serve process or a hung
// `opencode` invocation blocked the worker indefinitely while holding the machine-WIDE
// WorkerLock — which also stops `capture` from ever draining, so the whole tool goes quiet
// with no error anywhere. Generous on purpose: these are all local operations (a loopback
// request, a version print), so seconds-scale is already far beyond normal,
// and the values must never be tight enough to abort healthy work on a loaded machine.
//
// healthCheckBudget is the TOTAL wall clock waitReady (serve.go) will spend waiting for a freshly
// started `opencode serve` to print its address and answer an authenticated /api/info. Each
// individual probe derives its deadline from this budget, so a single wedged request cannot
// overrun it.
//
// Vars, not consts, for the same reason openCodeAsyncPollInterval is: a test must be able to
// shrink them to prove the bound actually fires, without waiting the real 30–60s.
var (
	openCodeRequestTimeout = 60 * time.Second
	openCodeProbeTimeout   = 60 * time.Second
	healthCheckBudget      = 30 * time.Second
	agentRewriteEvery      = 3 * time.Second // re-write config while an agent is unlisted
)

// Run sends one isolated distillation request through the private serve: one fresh session per
// job, created with the agent that carries systemPrompt, prompted with the corpus, polled until its
// turn is idle, then deleted. The prompt route returns at once, so no HTTP request stays open for
// the model's latency.
func (s *OpenCodeServer) Run(ctx context.Context, model, systemPrompt, input string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, requestLifetimeBudget())
	defer cancel()

	// Concurrency (issue #22): the lock guards ONLY the closed check, NOT the whole
	// request. Each Run creates its own isolated OpenCode session and shares no
	// per-request state; s.client is safe for concurrent use and baseURL/authHeader
	// are read-only after construction. A benchmark against a real `opencode serve`
	// confirmed the server accepts many concurrent isolated sessions (submit latency
	// stays flat as N rises), so holding the mutex across the whole request —
	// as this used to — needlessly serialized the engine's parallel drain. If Close
	// flips `closed` after we pass this check, the in-flight HTTP call just fails
	// against the shutting-down server and the session stays pending for retry — the
	// same graceful outcome as any transport error, no corruption. (s.sem bounds the
	// parallelism instead; see runSessionWithMessage.)
	s.mu.Lock()
	closed := s.closed
	s.mu.Unlock()
	if closed {
		return "", fmt.Errorf("opencode server is closed")
	}
	// Refuse an unnamed model before any request: an empty one would get OpenCode's default.
	if _, err := splitOpenCodeModel(model); err != nil {
		return "", err
	}
	agent, err := s.ensureAgent(ctx, systemPrompt)
	if err != nil {
		return "", err
	}
	sessionID, err := s.createSession(ctx, model, agent)
	if err != nil {
		return "", err
	}
	defer s.deleteSessionBestEffort(sessionID)
	return s.runSessionWithMessage(ctx, sessionID, "msg_"+randomHex(12), input)
}

// agentID names the agent for one system prompt: "w-" plus the first 16 hex characters of its
// sha256. Deterministic, so a resumed native session finds its agent again in the next serve's
// config, and two lenses with the same prompt share one agent.
func agentID(systemPrompt string) string {
	sum := sha256.Sum256([]byte(systemPrompt))
	return "w-" + hex.EncodeToString(sum[:])[:16]
}

// ensureAgent makes systemPrompt an agent of the running serve and returns its id.
//
// v2's session and prompt routes carry no system prompt (the only route that does is
// experimental, critic C1), and an env var cannot carry one: Windows caps a variable at 32,767
// chars and the lens prompts total ~36 KB. So each distinct prompt is an agent in the private config
// (writePrivateConfig: `agents.<id>.system`, deny-all). A running serve picks a new one up ~0.25 s
// after the atomic write, without a restart; until /api/agent lists it, a prompt fails with "Agent
// not found", so this waits for the listing.
//
// agentMu is held across the wait, so a concurrent caller never sees an agent before the serve has
// it; s.loaded is read before the lock, so a known agent never queues behind another one's wait.
// An agent that did not load within the budget keeps that error for this serve: a v2 release that
// stops loading the `agents` key would otherwise cost every pending (session, lens) another 30 s,
// one after another under the WorkerLock. The next Open's fresh serve tries again.
func (s *OpenCodeServer) ensureAgent(ctx context.Context, systemPrompt string) (string, error) {
	id := agentID(systemPrompt)
	known := func() (bool, error) {
		v, ok := s.loaded.Load(id)
		err, _ := v.(error)
		return ok, err
	}
	if ok, err := known(); ok {
		return id, err
	}
	s.agentMu.Lock()
	defer s.agentMu.Unlock()
	if ok, err := known(); ok {
		return id, err
	}
	s.agents[id] = systemPrompt
	if err := writePrivateConfig(s.configPath, s.agents); err != nil {
		delete(s.agents, id)
		return "", err
	}
	parent := ctx
	ctx, cancel := context.WithTimeout(ctx, healthCheckBudget)
	defer cancel()
	rewriteTicker := time.NewTicker(agentRewriteEvery)
	defer rewriteTicker.Stop()
	// lastErr keeps the poll failure, so a serve that died after readiness (connection refused) or
	// a 401/500 is reported as such, not as an agent that "did not load" (lastPollNote's reason).
	var lastErr error
	for {
		data, err := s.doJSON(ctx, http.MethodGet, "/api/agent", nil, http.StatusOK)
		if err == nil {
			var list struct {
				Data []struct {
					ID string `json:"id"`
				} `json:"data"`
			}
			if err = json.Unmarshal(data, &list); err == nil {
				for _, a := range list.Data {
					if a.ID == id {
						s.loaded.Store(id, nil)
						return id, nil
					}
				}
			}
		}
		if err != nil && (ctx.Err() == nil || lastErr == nil) { // a poll the budget cut off says only that
			lastErr = err
		}
		select {
		case <-ctx.Done():
			delete(s.agents, id)
			if parent.Err() != nil { // the caller gave up (worker stop, Ctrl-C): not a load failure
				return "", fmt.Errorf("stopped waiting for opencode to load witness's agent %s: %w", id, parent.Err())
			}
			err := fmt.Errorf("opencode did not load witness's agent %s within %s%s", id, healthCheckBudget, lastPollNote(lastErr))
			s.loaded.Store(id, err)
			return "", err
		case <-rewriteTicker.C:
			// A 2.2.7/Linux contract run once never listed a new agent within 30 s (its re-run passed),
			// as if the file watcher missed the write. Re-writing the same config re-fires the watcher;
			// a dead watcher still fails at the budget as before.
			if err := writePrivateConfig(s.configPath, s.agents); err != nil {
				lastErr = err
			}
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// runSessionWithMessage prompts an existing session with input under messageID and waits for the
// turn. Run's sessions and native's retained scratch sessions both come through here, so the
// generation cap covers both. On any failure it interrupts the turn, which aborts the upstream
// request (runner.md (c)). The caller owns the session: Run deletes it, native keeps it for
// finalize or the next attempt.
func (s *OpenCodeServer) runSessionWithMessage(ctx context.Context, sessionID, messageID, input string) (string, error) {
	// ponytail: 16-way verified on 2.2.7 and 2.3.0, Linux and Windows (TestContractRunnerSixteenParallel);
	// production keeps 8 pending the owner's decision
	select {
	case s.sem <- struct{}{}:
	case <-ctx.Done():
		return "", fmt.Errorf("wait for an opencode generation slot: %w", ctx.Err())
	}
	defer func() { <-s.sem }()
	// Log the request's SIZE before sending (the model and agent are on createSession's line,
	// keyed by the same session id).
	//
	// A generation that never completes reports only `context deadline exceeded`, which names the
	// deadline and describes nothing about what was asked. That blind spot cost three wrong
	// diagnoses on a real Windows failure. Note that the corpus is UNBOUNDED by default — chunking
	// is off (ChunkMaxChars=0 renders the whole session as one transcript) — so a session with very
	// few MESSAGES can still produce an enormous request when its turns contain tool output or file
	// dumps.
	corpusChars := utf8.RuneCountInString(input)
	slog.Info("opencode: prompting", "session", sessionID, "message", messageID, "corpus_chars", corpusChars)
	sent := time.Now()
	// The corpus alone: distill.RunnerMine already fenced it (and put CorpusNotice on the system
	// prompt, which travels as the session's agent), the one place every runner's fence is applied.
	body := map[string]string{"id": messageID, "text": input}
	_, err := s.doJSON(ctx, http.MethodPost, "/api/session/"+sessionID+"/prompt", body, http.StatusOK)
	var reply string
	if err == nil {
		reply, err = s.waitForReply(ctx, sessionID, messageID)
	}
	if err != nil {
		// A failed prompt POST is interrupted too: the serve may have recorded the turn before the
		// response failed, and a native session is retained, not deleted (spec 7.5: any error).
		// Attribute the failure to a SIZE and a DURATION, so a stall is diagnosable from the log
		// alone rather than by re-running with a debugger attached.
		slog.Warn("opencode: generation did not complete", "session", sessionID, "corpus_chars", corpusChars,
			"waited", time.Since(sent).String(), "err", err.Error())
		s.interruptBestEffort(sessionID)
		return "", err
	}
	slog.Info("opencode: generation complete", "session", sessionID, "corpus_chars", corpusChars,
		"reply_chars", utf8.RuneCountInString(reply), "took", time.Since(sent).String())
	return reply, nil
}

// replyForMessage harvests a retained turn (native resume): the reply once the turn is idle
// `succeeded`, else "". A turn with no idle never finishes — a private --stdio serve does not resume
// one after a restart (critic C3) — so the caller starts a fresh session rather than waiting on it.
func (s *OpenCodeServer) replyForMessage(ctx context.Context, sessionID, messageID string) (string, error) {
	data, err := s.doJSON(ctx, http.MethodGet, messagesPath(sessionID), nil, http.StatusOK)
	if err != nil {
		return "", err
	}
	st, err := parseTurn(data, messageID)
	if err != nil || st.Idle == nil || st.Idle.Outcome != "succeeded" {
		return "", err
	}
	return st.Reply, nil
}

// turnPageSize is how many messages each poll fetches, oldest first. A session holds only
// witness's own turns — our request, its assistant record(s), the idle — so our request is near
// the start; the page is generous so a resumed native session's earlier attempts still fit.
// requestMissingStreak below is the guard for that being wrong: rather than trust the page for the
// full generateTimeout, we surface it.
const turnPageSize = 200

// messagesPath is the ONE spelling of the poll URL, so both poll sites (waitForReply,
// replyForMessage) read the same page in the same order. The order must be asc: the default is
// newest-first, and parseTurn reads the records AFTER our request.
func messagesPath(sessionID string) string {
	return fmt.Sprintf("/api/session/%s/message?order=asc&limit=%d", sessionID, turnPageSize)
}

// requestMissingLimit is how many consecutive polls may come back WITHOUT our request message
// before we stop waiting. The serve records a prompt before the prompt route returns, so more
// than a handful of polls without it means it is not coming — the page does not reach it, the id
// was rejected, or the session was reset. Failing here yields an actionable error in seconds
// instead of a generateTimeout-long silence.
const requestMissingLimit = 10

// lastPollNote renders the most recent poll failure for inclusion in a phase-budget error, or "" when
// polling was healthy. Without it a request whose polls were ALL failing would be reported purely as
// a phase timeout, hiding the transport error that actually caused it.
func lastPollNote(lastErr error) string {
	if lastErr == nil {
		return ""
	}
	return fmt.Sprintf(" (every poll also failed; last: %v)", lastErr)
}

func (s *OpenCodeServer) waitForReply(ctx context.Context, sessionID, requestID string) (string, error) {
	ticker := time.NewTicker(openCodeAsyncPollInterval)
	defer ticker.Stop()
	var lastErr error
	requestMissingStreak := 0
	path := messagesPath(sessionID)

	// TWO-PHASE BUDGET: queue-wait and generation are bounded SEPARATELY.
	//
	// The caller's ctx carries generateTimeout, which used to run from the moment witness sent.
	// That silently charged provider QUEUE TIME as generation time, and with several requests in
	// flight against a provider that serves serially the ones at the back died having never been
	// generated. Measured: four simultaneous prompts, all four dead at exactly 600s; one solo
	// retry then completed in 41s. Four such generations total ~164s of real work — they fit
	// inside a SINGLE request's budget, so the concurrency was never the problem. Charging the
	// wait was.
	//
	// So the generation deadline now starts when the provider starts, and queueing gets its own,
	// much larger allowance. "Started" is turnState.Started: an assistant record whose `retry` is
	// null. v2 creates the assistant record at once on a retryable failure and keeps `retry` set
	// through its internal backoff (critic C5), so "an assistant exists" alone would charge that
	// backoff to the generation budget — the #143 bug in a new place. This keeps real concurrency —
	// which is the point of parallel mining — and stops a queued request from being killed for
	// waiting its turn.
	//
	// The ctx deadline still applies as the absolute ceiling for both phases together, so a
	// cancelled drain (Ctrl-C, `worker stop`) still tears down promptly.
	sent := time.Now()
	var startedAt time.Time
	for {
		data, err := s.doJSON(ctx, http.MethodGet, path, nil, http.StatusOK)
		var st turnState
		if err == nil {
			st, err = parseTurn(data, requestID)
		}
		if err == nil {
			// Our own request must show up; until it does, nothing here is attributable to this
			// run (parseTurn reads only records after it). If it NEVER shows up we must not keep
			// polling: that is a full generateTimeout of silence while holding the machine-wide
			// WorkerLock.
			if !st.Present {
				requestMissingStreak++
				if requestMissingStreak >= requestMissingLimit {
					return "", fmt.Errorf("opencode never echoed our prompt (message %s) into the first %d "+
						"messages of session %s after %d polls", requestID, turnPageSize, sessionID, requestMissingStreak)
				}
			} else {
				requestMissingStreak = 0
			}
			// Phase transition: note when generation actually began, so the generation
			// deadline is measured from there rather than from when we sent.
			if startedAt.IsZero() && st.Started {
				startedAt = time.Now()
				slog.Debug("opencode: generation started", "session", sessionID,
					"queued", time.Since(sent).String())
			}
			// FAIL FAST on a turn that ended badly (failed, interrupted, the free limit): the
			// provider already answered, so waiting or retrying here cannot help.
			if err := turnError(st, time.Now()); err != nil {
				return "", err
			}
			if st.Idle != nil {
				// Succeeded (turnError covers every other outcome). Only an idle ends a turn: the
				// assistant's text streams in, so a non-empty reply before it is still partial.
				if strings.TrimSpace(st.Reply) == "" {
					// The most likely cause is a model that reasoned only, or reached for a tool:
					// witness's agent denies all tools, so the turn can finish with no prose.
					return "", fmt.Errorf("opencode generation completed without emitting any text " +
						"(the model may have attempted a tool call, which witness's agent denies, or " +
						"refused silently); the prompt asks for a JSON array only")
				}
				return st.Reply, nil
			}
			// OpenCode scheduled its next attempt past what is left of the queue budget (a long
			// retry-after: it sleeps up to 15 min per attempt). Waiting it out cannot end in a
			// start inside the budget; it only holds the WorkerLock. Give up now and say why.
			if startedAt.IsZero() && st.Retry != nil {
				if at := time.UnixMilli(st.Retry.At); at.After(sent.Add(queueWaitBudget)) {
					return "", fmt.Errorf("opencode: the provider asked to retry at %s (%s: %s), past this "+
						"request's remaining queue budget; giving up: %w", at.Format(time.RFC3339),
						st.Retry.Error.Type, st.Retry.Error.Message, context.DeadlineExceeded)
				}
			}
		} else {
			lastErr = err
		}
		// Enforce the two phases on EVERY iteration — including one whose poll FAILED.
		//
		// This deliberately sits outside the `if err == nil` branch above. When it lived inside,
		// a poll that kept erroring was charged nothing: not the queue budget, not the generation
		// budget, and not the request-missing streak. A serve that accepts the prompt and then
		// fails or wedges every GET (a 500, or each GET burning openCodeRequestTimeout) would then
		// run to the blunt requestLifetimeBudget ctx ceiling — which the two-phase change RAISED
		// from generateTimeout to both phases together. So the fix for one stall would have made a
		// different stall several times worse. Charging time regardless of poll outcome is what
		// keeps the budgets honest, and lastErr is still reported so the message names the
		// transport failure rather than hiding it behind a phase.
		if startedAt.IsZero() {
			if waited := time.Since(sent); waited > queueWaitBudget {
				return "", fmt.Errorf("opencode never began generating after %s queued (the provider "+
					"is serving other requests; reduce mine_concurrency or wait)%s: %w",
					waited.Truncate(time.Second), lastPollNote(lastErr), context.DeadlineExceeded)
			}
		} else if gen := time.Since(startedAt); gen > generateTimeout {
			return "", fmt.Errorf("opencode generation exceeded %s after starting%s: %w",
				generateTimeout, lastPollNote(lastErr), context.DeadlineExceeded)
		}
		select {
		case <-ctx.Done():
			if lastErr != nil {
				return "", fmt.Errorf("wait for opencode reply: %w (last poll: %v)", ctx.Err(), lastErr)
			}
			return "", fmt.Errorf("wait for opencode reply: %w", ctx.Err())
		case <-ticker.C:
		}
	}
}

// interruptBestEffort ends a session's turn upstream (the serve aborts the provider request).
// Its own bound, because the caller's ctx is usually the one that just expired.
func (s *OpenCodeServer) interruptBestEffort(sessionID string) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := s.doJSON(ctx, http.MethodPost, "/api/session/"+sessionID+"/interrupt", nil, http.StatusOK, http.StatusNotFound); err != nil {
		slog.Warn("opencode: could not interrupt witness distill session", "session", sessionID, "err", err)
	}
}

// Close ends the private serve's lease (privateServe.Close). Idempotent, and a no-op for a server
// with no serve.
func (s *OpenCodeServer) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	s.mu.Unlock()
	if s.serve == nil {
		return nil
	}
	return s.serve.Close()
}

// createSession creates an empty session bound to agent and model. The serve does not validate
// the agent here; a missing one fails the first prompt (hence ensureAgent first).
func (s *OpenCodeServer) createSession(ctx context.Context, model, agent string) (string, error) {
	ref, err := splitOpenCodeModel(model)
	if err != nil {
		return "", err
	}
	data, err := s.doJSON(ctx, http.MethodPost, "/api/session", map[string]any{"title": "witness", "agent": agent, "model": ref, "permissions": denyAll}, http.StatusOK)
	if err != nil {
		return "", err
	}
	var resp struct {
		Data struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return "", fmt.Errorf("decode opencode session: %w", err)
	}
	if strings.TrimSpace(resp.Data.ID) == "" {
		return "", fmt.Errorf("opencode session response had no id")
	}
	slog.Info("opencode: session", "session", resp.Data.ID, "agent", agent, "model", model)
	return resp.Data.ID, nil
}

func (s *OpenCodeServer) deleteSessionBestEffort(id string) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := s.deleteSession(ctx, id); err != nil {
		slog.Warn("opencode: could not delete witness distill session", "session", id, "err", err)
	}
}

func (s *OpenCodeServer) deleteSession(ctx context.Context, id string) error {
	_, err := s.doJSON(ctx, http.MethodDelete, "/api/session/"+id, nil, http.StatusOK, http.StatusNoContent, http.StatusNotFound)
	return err
}

func (s *OpenCodeServer) doJSON(ctx context.Context, method, path string, body any, okStatuses ...int) ([]byte, error) {
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		r = bytes.NewReader(b)
	}
	// Bound every individual request. The caller's ctx is signal-cancellable but carries
	// no deadline, and the shared http.Client has no Timeout, so a serve process that
	// accepted the connection and then wedged left this blocked in Do() forever — pinning
	// the machine-wide WorkerLock until someone sent a signal by hand. The timeout goes
	// HERE rather than on the Client because generation is observed by POLLING: each poll
	// is a short request (bounded by this), while the overall wait is bounded separately by
	// generateTimeout. A Client.Timeout would apply the same ceiling to both.
	reqCtx, cancel := context.WithTimeout(ctx, openCodeRequestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, method, s.baseURL+path, r)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", s.authHeader)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	for _, want := range okStatuses {
		if resp.StatusCode == want {
			return data, nil
		}
	}
	return nil, fmt.Errorf("opencode %s %s failed: %s: %s", method, path, resp.Status, strings.TrimSpace(string(data)))
}

// ValidateOpenCodeCapability gates the private-serve protocol (OpenCode 2.2.7 up to, not
// including, 3.0). Do not fall back to a shared user DB: an unavailable capability leaves L0
// pending for retry. It takes no runtime root: ProbeVersion needs none (`--version` opens no DB).
func ValidateOpenCodeCapability(ctx context.Context) error {
	v, err := ProbeVersion(ctx)
	if err != nil {
		return err
	}
	return CheckVersion(v)
}

func modelHint(models []string) string {
	if len(models) == 0 {
		return " (that provider returned no models)"
	}
	limit := len(models)
	if limit > 12 {
		limit = 12
	}
	hint := "; available: " + strings.Join(models[:limit], ", ")
	if len(models) > limit {
		hint += ", ..."
	}
	return hint
}

// ErrOpenCodeNoModel refuses an unnamed stage: OpenCode's default model (opencode/big-pickle with
// free models on) may train on inputs, so witness never lets OpenCode choose.
var ErrOpenCodeNoModel = errors.New("the OpenCode runner needs an explicit model for every stage; it never falls back to OpenCode's default")

// splitOpenCodeModel turns `provider/model[#variant]` into v2's model ref. Only the first slash
// splits (openrouter ids carry their own), and `#variant` picks one of the model's variants.
func splitOpenCodeModel(model string) (map[string]string, error) {
	model = strings.TrimSpace(model)
	if model == "" {
		return nil, fmt.Errorf("%w; set one with `witness config set mine_model <provider/model>` (and review_model), "+
			"or a lens's extract_model/review_model; `opencode models` lists the ids", ErrOpenCodeNoModel)
	}
	ref, variant, hasVariant := strings.Cut(model, "#")
	provider, id, ok := strings.Cut(ref, "/")
	if !ok || provider == "" || id == "" || (hasVariant && variant == "") {
		return nil, fmt.Errorf("opencode model %q must use provider/model format (optionally #variant)", model)
	}
	out := map[string]string{"providerID": provider, "id": id}
	if hasVariant {
		out["variant"] = variant
	}
	return out, nil
}

func basicAuthToken(user, password string) string {
	return base64.StdEncoding.EncodeToString([]byte(user + ":" + password))
}

// randomHex is infallible: since Go 1.24 crypto/rand.Read never returns an error.
func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

type safeBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (b *safeBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.Write(p)
}

func (b *safeBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.String()
}
