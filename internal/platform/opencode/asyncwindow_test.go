package opencode

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// requestMissingLimit must be a real bound: small enough to beat generateTimeout by a wide
// margin (the whole point is an actionable error in seconds), and >1 so a single racy poll before
// the server echoes our request cannot abort a healthy run.
func TestRequestMissingLimitIsABoundNotAHairTrigger(t *testing.T) {
	if requestMissingLimit < 3 {
		t.Errorf("requestMissingLimit = %d is a hair trigger; the server needs a few polls to echo "+
			"our request", requestMissingLimit)
	}
	// At one poll per openCodeAsyncPollInterval, the guard must fire far sooner than the
	// liveness ceiling, or it adds nothing over just waiting.
	if budget := openCodeAsyncPollInterval * requestMissingLimit; budget >= generateTimeout/4 {
		t.Errorf("the guard needs %s to fire, which is not meaningfully faster than the %s "+
			"generateTimeout it exists to preempt", budget, generateTimeout)
	}
}

func readFileForTest(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	return normalizeOpenCodeNewlines(string(b))
}

// normalizeOpenCodeNewlines converts CRLF and lone CR to LF so this package's source scans see
// one line-ending form. Without it a CRLF checkout (git's Windows default) made the "\n}\n"
// body-delimiter scans miss entirely: three such tests panicked on the resulting negative slice
// bound and two silently stopped asserting. Reported from a real Windows run.
func normalizeOpenCodeNewlines(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	return strings.ReplaceAll(s, "\r", "\n")
}

// A turn that never contains our request must FAIL FAST, not spin to generateTimeout.
//
// parseTurn attributes nothing to this run until our own request is present, so a page without it
// is byte-for-byte "still waiting" — every poll, forever. Before this guard (in the v1 fork era,
// when our request could fall out of the fetched window) the poll kept going for the full
// generateTimeout and then blamed a timeout, which on a real Windows box read as "the free-tier
// model is too slow" and sent us tuning models instead of reading the code.
func TestWaitForReplyFailsFastWhenOurRequestNeverAppears(t *testing.T) {
	fake, srv := newFakeV2(t)
	fake.turn = func(*fakeSession) []map[string]any {
		return []map[string]any{{"id": "msg_hist1", "type": "user"}, assistantRec("b", nil)}
	}
	// A ctx far longer than the guard needs: if the guard does not fire, this test hangs until
	// here, which is the bug. Kept well under `go test`'s own timeout.
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	_, err := srv.Run(ctx, "openai/gpt-5.5", "P", "D")
	if err == nil || !strings.Contains(err.Error(), "never echoed our prompt") {
		t.Fatalf("err = %v, want our request named as never echoed", err)
	}
	if ctx.Err() != nil {
		t.Fatalf("spun until the context deadline instead of failing fast: %v", err)
	}
	polls := fake.count(func(c string) bool { return strings.HasSuffix(c, "/message") })
	if polls < 2 || polls > requestMissingLimit+2 {
		t.Errorf("gave up after %d polls for a limit of %d; one racy poll must not abort a healthy run, "+
			"and the bound must apply", polls, requestMissingLimit)
	}
	assertInterruptedThenDeleted(t, fake.seen(), "ses_1")
}

// The distillation context must be seeded ONLY by witness's own turns — never by inheriting a
// conversation witness did not author.
//
// This is the invariant every other stage already honours and that mining silently broke:
//   - the Claude runner runs `claude -p --no-session-persistence`: a stateless one-shot process
//     with zero conversation context (claude/runner.go);
//   - every OpenCode stage, mining included, takes server.go Run: it creates a session, prompts
//     it, deletes it.
//
// Mining once broke it: the removed native-session path forked the user's session, so it started
// with ~the whole conversation already in the model's context. That cost three things and bought
// nothing: it contradicted the "treat the user turn as untrusted" system prompt, it pushed
// witness's own request outside the reply window on a long session — producing the `context
// deadline exceeded` that made Windows distillation look like a slow model — and it re-sent the
// whole history to the provider on every call.
//
// A source scan, because the property is "no code path forks", which no single behavioural test
// can establish. The behavioural half is TestRunCreatesPromptsAndDeletesOneSessionPerJob,
// whose fake (fakeV2) answers `POST /api/session` and fails on any route it does not know.
func TestNoCodePathForksAConversation(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	scanned := 0
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src := readFileForTest(t, f)
		scanned++
		for n, line := range strings.Split(src, "\n") {
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "//") {
				continue // prose about the old design is fine; a live request is not
			}
			// The OpenCode fork endpoint, in any spelling that would actually issue one.
			if strings.Contains(line, `"/fork"`) || strings.Contains(line, `/fork"`) {
				t.Errorf("%s:%d issues an OpenCode fork request: %s\n"+
					"A distillation context must contain only witness's own turns — inheriting the "+
					"user's conversation is what buried our request in the reply window.", f, n+1, trimmed)
			}
		}
	}
	if scanned == 0 {
		t.Fatal("scanned no files — the glob is wrong and this test is vacuous")
	}
}

// A turn that SUCCEEDS without emitting text must fail fast with that outcome named, not be
// returned as an empty reply or waited on.
//
// Only `text` parts are the reply; reasoning parts are dropped (every free model is a reasoning
// model, freetier §6). So a turn whose assistant only reasoned — or reached for a tool, which
// witness's deny-all agent refuses — ends idle `succeeded` with nothing usable. In v1 that shape
// was indistinguishable from "still generating" and burned the full generateTimeout; it was
// misdiagnosed three times (rate limits, the reply window, the session fork). Retrying cannot help.
func TestCompletedWithoutTextFailsFast(t *testing.T) {
	fake, srv := newFakeV2(t)
	fake.turn = func(s *fakeSession) []map[string]any {
		a := map[string]any{"type": "assistant", "content": []map[string]any{{"type": "reasoning", "text": "hmm"}}}
		return []map[string]any{s.user(), a, idleRec("succeeded", nil)}
	}
	_, err := srv.Run(context.Background(), "openai/gpt-5.5", "P", "D")
	if err == nil || !strings.Contains(err.Error(), "without emitting any text") {
		t.Fatalf("err = %v, want the textless completion named", err)
	}
	if polls := fake.count(func(c string) bool { return strings.HasSuffix(c, "/message") }); polls > 1 {
		t.Errorf("took %d polls to notice a finished turn; it is visible on the first one", polls)
	}
}

// The #143 budget, v2 shape: a request OpenCode is still retrying has not started.
//
// Reported from a real run (v1): witness fired four simultaneous prompts and all four died at
// exactly the 600 s budget, then one solo retry completed in 41 s. Four such generations are ~164 s
// of real work, so concurrency was never too much for the provider; the deadline started when
// witness SENT, so a request waiting its turn was charged as generating. In v2 the same bug has a
// new hiding place (runner.md (c)(3)): on any retryable failure the assistant record appears AT
// ONCE, with `retry` set, while OpenCode backs off internally for up to ~85 s (or 10 x 15 min on
// a long retry-after). "An assistant exists" alone would charge that backoff to the generation
// budget, so "started" is assistant && retry == null (critic C5).
//
// The fake here IS that provider: it serves one request at a time; the others show an assistant in
// backoff until their turn. The last waiter queues ~450 ms, past the 400 ms generation budget.
func TestQueueTimeIsNotChargedAsGeneration(t *testing.T) {
	prev := generateTimeout
	generateTimeout = 400 * time.Millisecond
	t.Cleanup(func() { generateTimeout = prev })
	fake, srv := newFakeV2(t)
	var queue []string // arrival order; only the head generates (turn runs under fake.mu)
	started, done := map[string]time.Time{}, map[string]bool{}
	fake.turn = func(s *fakeSession) []map[string]any {
		if !done[s.ID] && !slices.Contains(queue, s.ID) {
			queue = append(queue, s.ID)
		}
		if len(queue) > 0 {
			if head := queue[0]; started[head].IsZero() {
				started[head] = time.Now()
			} else if time.Since(started[head]) > 150*time.Millisecond {
				done[head], queue = true, queue[1:]
			}
		}
		switch {
		case done[s.ID]:
			return okTurn("[]")(s)
		case !started[s.ID].IsZero():
			return []map[string]any{s.user(), assistantRec("", nil)} // generating, no text yet
		default:
			return []map[string]any{s.user(), assistantRec("", retryRec(time.Now().Add(time.Second), "provider.rate-limit", "busy"))}
		}
	}
	var wg sync.WaitGroup
	errs := make([]error, 4)
	for i := range errs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second) // a stuck slot fails, not hangs
			defer cancel()
			_, errs[i] = srv.Run(ctx, "openai/gpt-5.5", "P", fmt.Sprint(i))
		}()
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Errorf("request %d failed against a serially-serving provider: %v\n"+
				"OpenCode's backoff is queue time; charging it as generation kills the waiters.", i, err)
		}
	}
}

// A retry the provider scheduled past the remaining queue budget will not start in time, so
// waiting it out only holds the WorkerLock: give up at once and say when the provider would retry.
// The measured case: a 429 with retry-after 3600 makes OpenCode schedule the next attempt 15 min
// out (runner.md (c)(3) r429l).
func TestQueuedRetryPastTheBudgetGivesUpEarly(t *testing.T) {
	prev := queueWaitBudget
	queueWaitBudget = 2 * time.Second
	t.Cleanup(func() { queueWaitBudget = prev })
	fake, srv := newFakeV2(t)
	fake.turn = func(s *fakeSession) []map[string]any {
		at := s.Prompted.Add(15 * time.Minute)
		return []map[string]any{s.user(), assistantRec("", retryRec(at, "provider.rate-limit", "mock rate limit"))}
	}
	// Bounded, so a regression that waits the retry out fails here instead of hanging the suite.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	start := time.Now()
	_, err := srv.Run(ctx, "openai/gpt-5.5", "P", "D")
	if d := time.Since(start); d >= time.Second {
		t.Errorf("took %s; a retry scheduled past the 2 s queue budget must give up at once", d)
	}
	if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "retry at") {
		t.Fatalf("err = %v, want a DeadlineExceeded naming the provider's retry time", err)
	}
	assertInterruptedThenDeleted(t, fake.seen(), "ses_1")
}

// The half the early give-up could break: a retry due INSIDE the budget is waited on.
func TestRetryWithinTheBudgetKeepsWaiting(t *testing.T) {
	prev := queueWaitBudget
	queueWaitBudget = 2 * time.Second
	t.Cleanup(func() { queueWaitBudget = prev })
	fake, srv := newFakeV2(t)
	fake.turn = func(s *fakeSession) []map[string]any {
		if at := s.Prompted.Add(100 * time.Millisecond); time.Now().Before(at) {
			return []map[string]any{s.user(), assistantRec("", retryRec(at, "provider.rate-limit", "mock rate limit"))}
		}
		return okTurn("RESULT")(s)
	}
	if got, err := srv.Run(context.Background(), "openai/gpt-5.5", "P", "D"); err != nil || got != "RESULT" {
		t.Fatalf("Run = %q, %v; want RESULT once the retry succeeds", got, err)
	}
}

// The GENERATION deadline must still bite once generation has actually started — the two-phase
// budget must not become "wait forever" — and it ends the turn upstream before deleting it.
func TestGenerationDeadlineStillAppliesOnceStarted(t *testing.T) {
	prev := generateTimeout
	generateTimeout = 200 * time.Millisecond
	t.Cleanup(func() { generateTimeout = prev })
	fake, srv := newFakeV2(t)
	fake.turn = func(s *fakeSession) []map[string]any {
		return []map[string]any{s.user(), assistantRec("", nil)} // started, never idle
	}
	// The ctx is far longer than the shrunk generation budget, so whichever fires first tells us
	// which mechanism bounded the request: the GENERATION phase must, not the ctx, and not the
	// 30-minute queue allowance (which a turn never marked as started would fall through to).
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err := srv.Run(ctx, "openai/gpt-5.5", "P", "D")
	if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "generation exceeded") {
		t.Fatalf("err = %v, want the generation phase's DeadlineExceeded", err)
	}
	if ctx.Err() != nil {
		t.Fatalf("ran to the context deadline instead of the generation budget: %v", err)
	}
	assertInterruptedThenDeleted(t, fake.seen(), "ses_1")
}

// A serve that accepts the prompt and then FAILS every poll must still be bounded by the phase
// budgets — not left to run to the blunt ctx ceiling.
//
// Found by an adversarial audit of the two-phase change, and it is the sharpest kind of finding:
// the fix for one stall made a DIFFERENT stall worse. Every phase check originally sat inside the
// `if err == nil` branch, so a poll that kept erroring was charged nothing — not the queue budget,
// not the generation budget, not the request-missing streak — and ran to requestLifetimeBudget.
func TestPollFailuresAreStillChargedAgainstThePhaseBudget(t *testing.T) {
	prev := queueWaitBudget
	queueWaitBudget = 200 * time.Millisecond
	t.Cleanup(func() { queueWaitBudget = prev })
	fake, srv := newFakeV2(t)
	fake.turn = func(*fakeSession) []map[string]any { return nil } // every poll 500s
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	_, err := srv.Run(ctx, "openai/gpt-5.5", "P", "D")
	if err == nil {
		t.Fatal("a serve that fails every poll must be an error")
	}
	if ctx.Err() != nil {
		t.Fatalf("ran to the context deadline instead of a phase budget: %v", err)
	}
	if polls := fake.count(func(c string) bool { return strings.HasSuffix(c, "/message") }); polls < 2 {
		t.Errorf("gave up after %d poll(s); a single transient 500 must not abort a request", polls)
	}
	// The error must NAME the transport failure. Reporting only a phase timeout would hide the
	// actual cause, which is the mistake this whole investigation kept making.
	if !strings.Contains(err.Error(), "every poll also failed") {
		t.Errorf("the error must surface the underlying poll failure; got %v", err)
	}
}

// lastPollNote must stay silent when polling was healthy, or every ordinary phase timeout would carry
// a misleading "(every poll also failed)" clause.
func TestLastPollNoteIsSilentOnHealthyPolling(t *testing.T) {
	if got := lastPollNote(nil); got != "" {
		t.Errorf("lastPollNote(nil) = %q, want empty", got)
	}
	if got := lastPollNote(fmt.Errorf("boom")); !strings.Contains(got, "boom") {
		t.Errorf("lastPollNote must include the error; got %q", got)
	}
}
