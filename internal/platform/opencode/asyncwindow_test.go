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
//   - the plain OpenCode path creates a session, prompts it, deletes it (server.go Run);
//   - L2 review, L4 profile and `witness lens try` all take that plain path, because
//     platform.WithNativeSession has exactly ONE producer (distill/worker.go, in MineSession).
//
// Only native MINING forked the user's session, so it alone started with ~the whole conversation
// already in the model's context. That cost three things and bought nothing (see the comment at
// the createSession call in native.go): it contradicted the "treat the user turn as untrusted"
// system prompt, it pushed witness's own request outside the reply window on a long session —
// producing the `context deadline exceeded` that made Windows distillation look like a slow model
// — and it re-sent the whole history to the provider on every call.
//
// A source scan, because the property is "no code path forks", which no single behavioural test
// can establish. The behavioural half is TestNativeRunUsesADistinctFreshScratchSessionPerJob,
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

// Missing credentials must be LOUD, and must be searched for in more than one place.
//
// This is the defect that made an unauthenticated isolated serve indistinguishable from a slow
// model. witness points the serve's XDG_DATA_HOME at its own runtime root, so the serve cannot see
// the user's OpenCode data dir; credentials are there only because prepareAuth copied them. The old
// code probed exactly ONE path — beside the database, which is the Unix layout — and returned nil
// when it was absent. The serve then came up with no credentials and provider requests were accepted
// and never completed: no error, no reply, a stall to the full 10-minute generateTimeout at ANY
// prompt size, including a 40-character one. Several diagnostic rounds were spent on that shape.
func TestFindOpenCodeAuthSearchesMoreThanTheUnixPath(t *testing.T) {
	// A HOME with no auth.json anywhere: the miss must be reported, with the paths searched.
	empty := t.TempDir()
	t.Setenv("HOME", empty)
	t.Setenv("USERPROFILE", empty) // Windows: os.UserHomeDir reads this
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("APPDATA", "")
	t.Setenv("LOCALAPPDATA", "")
	t.Setenv("WITNESS_OPENCODE_DB", filepath.Join(empty, "opencode", "opencode.db"))

	path, searched := findOpenCodeAuth()
	if path != "" {
		t.Fatalf("found auth at %q in an empty HOME", path)
	}
	if len(searched) < 2 {
		t.Errorf("searched only %v — one probe is what let a Windows layout go unnoticed; the miss "+
			"must be reported against several plausible locations", searched)
	}
	// The report must be actionable: it has to name the canonical location.
	var namesDBDir bool
	for _, s := range searched {
		if strings.Contains(s, "opencode") && strings.HasSuffix(s, "auth.json") {
			namesDBDir = true
		}
	}
	if !namesDBDir {
		t.Errorf("the searched list %v does not name an opencode auth.json path, so the warning "+
			"cannot tell the user where to look", searched)
	}
}

// Each supported location must actually be honoured — a candidate list that never matches is worse
// than one probe, because it reads as thorough.
func TestFindOpenCodeAuthHonoursEachLocation(t *testing.T) {
	for _, tc := range []struct{ name, env string }{
		{"beside the database (Unix layout)", "db"},
		{"XDG_CONFIG_HOME", "XDG_CONFIG_HOME"},
		{"APPDATA (Windows)", "APPDATA"},
		{"LOCALAPPDATA (Windows)", "LOCALAPPDATA"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			t.Setenv("USERPROFILE", home)
			t.Setenv("XDG_CONFIG_HOME", "")
			t.Setenv("APPDATA", "")
			t.Setenv("LOCALAPPDATA", "")
			t.Setenv("WITNESS_OPENCODE_DB", filepath.Join(home, "nowhere", "opencode.db"))

			var want string
			if tc.env == "db" {
				dir := filepath.Join(home, "dbdir")
				t.Setenv("WITNESS_OPENCODE_DB", filepath.Join(dir, "opencode.db"))
				want = filepath.Join(dir, "auth.json")
			} else {
				dir := filepath.Join(home, tc.env)
				t.Setenv(tc.env, dir)
				want = filepath.Join(dir, "opencode", "auth.json")
			}
			if err := os.MkdirAll(filepath.Dir(want), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(want, []byte(`{"anthropic":{"type":"api","key":"k"}}`), 0o600); err != nil {
				t.Fatal(err)
			}

			got, searched := findOpenCodeAuth()
			if got != want {
				t.Errorf("auth at %s was not found (got %q); searched %v", tc.name, got, searched)
			}
		})
	}
}

// An EMPTY auth.json must not count as credentials: a zero-byte file left by a failed write would
// otherwise satisfy the probe, get copied, and reproduce the unauthenticated stall while looking
// like success.
func TestFindOpenCodeAuthIgnoresAnEmptyFile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("APPDATA", "")
	t.Setenv("LOCALAPPDATA", "")
	dir := filepath.Join(home, "dbdir")
	t.Setenv("WITNESS_OPENCODE_DB", filepath.Join(dir, "opencode.db"))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "auth.json"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if got, _ := findOpenCodeAuth(); got != "" {
		t.Errorf("an empty auth.json was accepted as credentials: %q", got)
	}
}

// prepareAuth must stay NON-FATAL on a miss (environment-backed providers like ANTHROPIC_API_KEY are
// a legitimate configuration with no auth.json), while still not pretending it copied anything.
func TestPrepareAuthIsNonFatalButCopiesNothingWhenAuthIsAbsent(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("APPDATA", "")
	t.Setenv("LOCALAPPDATA", "")
	t.Setenv("WITNESS_OPENCODE_DB", filepath.Join(home, "opencode", "opencode.db"))

	root := t.TempDir()
	n := newNativeRuntime(root, nil)
	if err := n.prepareAuth(); err != nil {
		t.Fatalf("a missing auth.json must not fail Open — env-backed providers need none: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "xdg", "opencode", "auth.json")); !os.IsNotExist(err) {
		t.Errorf("prepareAuth materialized an auth.json it never found (err=%v)", err)
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
			_, errs[i] = srv.Run(context.Background(), "openai/gpt-5.5", "P", fmt.Sprint(i))
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
