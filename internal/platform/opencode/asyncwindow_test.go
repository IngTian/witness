package opencode

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// The reply window must be large enough, and its size must be USED — both poll sites.
//
// witness prompts a FORK of the user's session, and a fork carries that whole conversation
// (parseOpenCodeAsyncReply says so, and TestAsyncProviderErrorIgnoresHistoryAndPreRequest pins the
// consequence). So the window is a tail of a conversation that may already be hundreds of messages
// long. At limit=20, a session with a long tail of tool/assistant messages after our prompt could
// push our own request out of the window — and parseOpenCodeAsyncReply returns "" whenever the
// request is absent, so the poll would spin silently until generateTimeout while holding the
// machine-wide WorkerLock. That is indistinguishable from "the model is slow", which is exactly how
// it was misdiagnosed as a rate-limit problem on a real Windows box.
func TestAsyncReplyWindowIsBigEnoughAndUsedEverywhere(t *testing.T) {
	if asyncReplyWindow <= 20 {
		t.Errorf("asyncReplyWindow = %d; a fork carries the source conversation, so the window must "+
			"comfortably exceed our request + the reply", asyncReplyWindow)
	}
	src := readFileForTest(t, "server.go")
	// No poll site may hardcode a limit; both must derive it from the constant.
	if strings.Contains(src, "message?limit=20") {
		t.Error("a poll site still hardcodes ?limit=20 instead of using asyncReplyWindow")
	}
	// Both poll sites (waitForAsyncReply, replyForMessage) must reference the constant.
	if n := strings.Count(src, "asyncReplyWindow"); n < 3 { // 1 declaration + 2 uses
		t.Errorf("asyncReplyWindow appears %d times; both poll sites must use it", n)
	}
}

// A window that never contains our request must FAIL FAST, not poll to the timeout.
//
// This is the guard for the assumption above being wrong (the window is not a tail, the id was
// rejected, the session was reset). Without it the symptom is a full generateTimeout of silence
// with no error — the worst possible failure shape, because it looks like a slow provider and
// sends you off tuning models instead of reading the code.
func TestHasOpenCodeRequestMessageDetectsAMissingRequest(t *testing.T) {
	// A history window that does NOT contain our request.
	history := `[{"info":{"id":"msg_old1","role":"user"},"parts":[{"type":"text","text":"a"}]},` +
		`{"info":{"id":"msg_old2","role":"assistant"},"parts":[{"type":"text","text":"b"}]}]`
	if hasOpenCodeRequestMessage([]byte(history), "msg_ours") {
		t.Error("our request was reported present in a window that does not contain it — the " +
			"fail-fast guard would never fire and the poll would spin to the timeout")
	}
	// The same window WITH our request must be recognized.
	withOurs := `[{"info":{"id":"msg_old1","role":"user"},"parts":[]},` +
		`{"info":{"id":"msg_ours","role":"user"},"parts":[{"type":"text","text":"go"}]}]`
	if !hasOpenCodeRequestMessage([]byte(withOurs), "msg_ours") {
		t.Error("our request was NOT found in a window that contains it — every poll would count as " +
			"missing and a healthy generation would be aborted")
	}
	// Degenerate inputs must not panic and must not claim presence.
	for _, bad := range []string{"", "null", "{}", "[]", "not json", `[{"info":{}}]`} {
		if hasOpenCodeRequestMessage([]byte(bad), "msg_ours") {
			t.Errorf("claimed our request is present in %q", bad)
		}
	}
	// An empty request id can never match anything.
	if hasOpenCodeRequestMessage([]byte(withOurs), "") {
		t.Error("an empty request id must never match")
	}
}

// The two parsers must agree about what counts as "our request", or the fail-fast guard fires on
// generations that are in fact progressing normally.
func TestRequestPresenceAgreesWithTheReplyParser(t *testing.T) {
	// Our request followed by an assistant reply: the parser returns the reply, and the presence
	// check must agree the request is there.
	data := `[{"info":{"id":"msg_ours","role":"user"},"parts":[{"type":"text","text":"go"}]},` +
		`{"info":{"id":"msg_r","role":"assistant"},"parts":[{"type":"text","text":"[]"}]}]`
	if reply := parseOpenCodeAsyncReply([]byte(data), "msg_ours"); strings.TrimSpace(reply) != "[]" {
		t.Fatalf("precondition: the parser should find the reply, got %q", reply)
	}
	if !hasOpenCodeRequestMessage([]byte(data), "msg_ours") {
		t.Error("the parser found our reply but the presence check says the request is absent — " +
			"the guard would abort a healthy generation")
	}

	// And the pre-request case: the parser must withhold, and presence must be true (our request
	// IS in the window), so the guard does NOT fire while we simply wait.
	pending := `[{"info":{"id":"msg_ours","role":"user"},"parts":[{"type":"text","text":"go"}]}]`
	if reply := parseOpenCodeAsyncReply([]byte(pending), "msg_ours"); reply != "" {
		t.Errorf("no assistant message yet, so the parser must return empty, got %q", reply)
	}
	if !hasOpenCodeRequestMessage([]byte(pending), "msg_ours") {
		t.Error("our request is present but reported missing — the guard would abort a generation " +
			"that has merely not finished yet")
	}
}

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

// A fork whose window never contains our request must FAIL FAST, not spin to generateTimeout.
//
// This is the behavioral half of the window guard, and it reproduces the misdiagnosis it exists to
// prevent. witness prompts a FORK of the user's session, so the message list starts with that whole
// conversation. If our own request never lands in the fetched window, parseOpenCodeAsyncReply
// correctly returns "" — every poll, forever. Before the guard, waitForAsyncReply kept polling for
// the full 10-minute generateTimeout and then blamed a timeout, which on a real Windows box read as
// "the free-tier model is too slow" and sent us tuning models instead of reading the code.
//
// The server here always answers with history that excludes our request — the shape a too-small or
// non-tail window produces on a long session.
func TestWaitForAsyncReplyFailsFastWhenOurRequestNeverAppears(t *testing.T) {
	polls := 0
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		polls++
		// Only ever the source conversation; our request id is nowhere in it.
		_, _ = w.Write([]byte(`[{"info":{"id":"msg_hist1","role":"user"},"parts":[{"type":"text","text":"a"}]},` +
			`{"info":{"id":"msg_hist2","role":"assistant"},"parts":[{"type":"text","text":"b"}]}]`))
	})
	ts := httptest.NewServer(h)
	defer ts.Close()
	srv := &OpenCodeServer{baseURL: ts.URL, authHeader: "Basic test", client: ts.Client()}

	// Poll fast so the test is quick; the guard counts POLLS, not wall clock.
	prev := openCodeAsyncPollInterval
	openCodeAsyncPollInterval = time.Millisecond
	defer func() { openCodeAsyncPollInterval = prev }()

	// A ctx deadline far longer than the guard needs: if the guard does not fire, this test
	// hangs until here, which is the bug. Kept well under `go test`'s own timeout.
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	start := time.Now()
	_, err := srv.waitForAsyncReply(ctx, "sess", "msg_ours")
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("a window that never contains our request must be an error, not a silent success")
	}
	if ctx.Err() != nil {
		t.Fatalf("waitForAsyncReply spun until the context deadline (%s) instead of failing fast — "+
			"in production that is a full generateTimeout of silence misread as a slow model: %v",
			elapsed, err)
	}
	// The message must name the actual problem, since diagnosing this from logs is the point.
	if !strings.Contains(err.Error(), "never echoed our prompt") {
		t.Errorf("the error must say our request never appeared; got %v", err)
	}
	if polls < 2 {
		t.Errorf("gave up after %d poll(s); a single racy poll must not abort a healthy run", polls)
	}
	if polls > requestMissingLimit+2 {
		t.Errorf("polled %d times for a limit of %d — the bound is not being applied", polls, requestMissingLimit)
	}
}

// The counter must RESET once our request shows up, or a slow generation that took a few polls to
// be echoed would be aborted mid-flight — turning the fix into a new failure.
func TestWaitForAsyncReplyWaitsOnceOurRequestIsPresent(t *testing.T) {
	polls := 0
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		polls++
		switch {
		case polls <= 3:
			// Not echoed yet — the guard must tolerate this.
			_, _ = w.Write([]byte(`[{"info":{"id":"msg_hist","role":"assistant"},"parts":[]}]`))
		case polls <= 6:
			// Echoed, still generating: no assistant message after ours.
			_, _ = w.Write([]byte(`[{"info":{"id":"msg_ours","role":"user"},"parts":[{"type":"text","text":"go"}]}]`))
		default:
			_, _ = w.Write([]byte(`[{"info":{"id":"msg_ours","role":"user"},"parts":[{"type":"text","text":"go"}]},` +
				`{"info":{"id":"msg_r","role":"assistant"},"parts":[{"type":"text","text":"[{\"observation\":\"x\"}]"}]}]`))
		}
	})
	ts := httptest.NewServer(h)
	defer ts.Close()
	srv := &OpenCodeServer{baseURL: ts.URL, authHeader: "Basic test", client: ts.Client()}

	prev := openCodeAsyncPollInterval
	openCodeAsyncPollInterval = time.Millisecond
	defer func() { openCodeAsyncPollInterval = prev }()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	reply, err := srv.waitForAsyncReply(ctx, "sess", "msg_ours")
	if err != nil {
		t.Fatalf("a generation whose request was echoed after a few polls must still be awaited: %v", err)
	}
	if !strings.Contains(reply, "observation") {
		t.Errorf("got %q, want the assistant reply", reply)
	}
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
// whose fake server answers `POST /session` and would fail on a fork request.
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

// The scratch session must be created, prompted and (after L1 is durable) deleted — the same
// lifecycle the plain path uses. This pins that native mining calls createSession, so a future
// change cannot quietly reintroduce a fork-shaped call under a different name.
func TestNativeMiningCreatesItsOwnScratchSession(t *testing.T) {
	src := readFileForTest(t, "native.go")
	i := strings.Index(src, "func (n *nativeRuntime) run(")
	if i < 0 {
		t.Fatal("nativeRuntime.run not found")
	}
	end := strings.Index(src[i:], "\n}\n")
	if end < 0 {
		t.Fatal("could not delimit nativeRuntime.run")
	}
	fn := src[i : i+end]

	if !strings.Contains(fn, "n.server.createSession(ctx, model)") {
		t.Error("native mining must create a FRESH session (n.server.createSession) — the same call " +
			"the plain OpenCode path, L2 review, L4 profile and `lens try` all already use")
	}
	if strings.Contains(fn, "n.server.fork(") {
		t.Error("native mining forks the user's conversation again")
	}
	if strings.Contains(fn, "importSnapshot(") {
		t.Error("the snapshot is imported again; that step existed only to give fork() a parent, and " +
			"the digest check reads the exported BYTES, before any import")
	}
	// The export MUST remain: its digest is what refuses to distill a session the user changed
	// after L0 capture. Dropping it silently would remove that guard.
	if !strings.Contains(fn, "n.export(ctx, w, snap)") {
		t.Error("the export was removed; validateExportDigest is the only check that L0 still " +
			"matches the user's session (`opencode native session changed after L0 capture`)")
	}
}

// A generation that COMPLETES without emitting text must fail fast, not poll to the deadline.
//
// This is the second invisible-completion shape, and the one the Windows reports kept landing on.
// parseOpenCodeAsyncReply harvests only `type:"text"` parts, so an assistant message that finished
// with TOOL-CALL parts (or none) yields "" — byte-for-byte what "still generating" looks like. The
// poll then burned the full 10-minute generateTimeout and blamed `context deadline exceeded`, which
// is why the cause was misdiagnosed three times (rate limits, the reply window, the session fork).
//
// Why it happens: witness's distillation agent sets permission {"*": "deny"}, so a model that
// reaches for a tool has the call denied and can end its turn having said nothing. Invisible on a
// trivial prompt (nothing to reach for), likely on a transcript full of file contents and tool
// output — exactly the reported split, where a hand-built "2+2=4" probe answered in 3s while every
// real code-reading session timed out.
func TestCompletedWithoutTextIsDetected(t *testing.T) {
	const req = `{"info":{"id":"msg_ours","role":"user","time":{"created":1}},"parts":[{"type":"text","text":"go"}]}`

	// A completed assistant turn whose only part is a TOOL CALL — the denied-tool shape.
	toolOnly := `{"info":{"id":"msg_r","role":"assistant","time":{"created":2,"completed":3}},` +
		`"parts":[{"type":"tool","tool":"read","state":{"status":"error"}}]}`
	if !completedWithoutText([]byte("["+req+","+toolOnly+"]"), "msg_ours") {
		t.Error("a completed assistant message with only tool parts was not detected — the poll " +
			"would spin to generateTimeout and blame the deadline")
	}

	// Completed with NO parts at all (the verbatim shape in testdata/async_provider_error.json).
	empty := `{"info":{"id":"msg_r","role":"assistant","time":{"created":2,"completed":3}},"parts":[]}`
	if !completedWithoutText([]byte("["+req+","+empty+"]"), "msg_ours") {
		t.Error("a completed assistant message with no parts was not detected")
	}

	// STILL GENERATING must NOT be reported: no completion timestamp. This is the half the fix
	// could break — a false positive here aborts every healthy generation mid-flight.
	pending := `{"info":{"id":"msg_r","role":"assistant","time":{"created":2}},"parts":[]}`
	if completedWithoutText([]byte("["+req+","+pending+"]"), "msg_ours") {
		t.Error("an in-flight generation was reported as completed-without-text; every slow but " +
			"healthy model call would be aborted")
	}

	// A real reply must NOT trip it, even when a tool part precedes the text.
	withText := `{"info":{"id":"msg_r","role":"assistant","time":{"created":2,"completed":3}},` +
		`"parts":[{"type":"tool","tool":"read"},{"type":"text","text":"[]"}]}`
	if completedWithoutText([]byte("["+req+","+withText+"]"), "msg_ours") {
		t.Error("a completed reply that DID emit text was reported as textless")
	}

	// History BEFORE our request is never this run's outcome — the same rule the reply and error
	// parsers follow. A textless completed message ahead of our request must be ignored.
	history := `{"info":{"id":"msg_old","role":"assistant","time":{"created":0,"completed":1}},"parts":[]}`
	if completedWithoutText([]byte("["+history+","+req+"]"), "msg_ours") {
		t.Error("a textless message PRECEDING our request was attributed to this run")
	}
	// And with our request absent entirely, nothing is attributable yet.
	if completedWithoutText([]byte("["+history+"]"), "msg_ours") {
		t.Error("a verdict was reached before our request even appeared")
	}

	// Degenerate inputs must not panic or claim a completion.
	for _, bad := range []string{"", "null", "{}", "[]", "not json", `[{"info":{}}]`} {
		if completedWithoutText([]byte(bad), "msg_ours") {
			t.Errorf("claimed completed-without-text for %q", bad)
		}
	}
}

// The real 1.18.3 capture must be classified as completed — that fixture is the ground truth for
// the info.time.completed shape this detector depends on. If OpenCode ever changes it, this fails
// here rather than silently reverting the fix to a 10-minute stall.
func TestOpenCodeMessageCompletedMatchesTheRealCaptureShape(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("testdata", "async_provider_error.json"))
	if err != nil {
		t.Fatal(err)
	}
	var list []json.RawMessage
	if err := json.Unmarshal(data, &list); err != nil {
		t.Fatal(err)
	}
	var sawCompleted, sawPending bool
	for _, m := range list {
		if openCodeMessageCompleted(m) {
			sawCompleted = true
		} else {
			sawPending = true
		}
	}
	if !sawCompleted {
		t.Error("no message in the real capture was seen as completed — info.time.completed is not " +
			"being read, so completedWithoutText can never fire and the stall returns")
	}
	if !sawPending {
		t.Error("every message read as completed, including the request; the check is too loose")
	}
}

// The poll must SURFACE it, not just detect it. Behavioural half, against a fake serve that
// answers with a completed-but-textless assistant turn forever.
func TestWaitForAsyncReplyFailsFastOnACompletedTextlessGeneration(t *testing.T) {
	polls := 0
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		polls++
		_, _ = w.Write([]byte(`[{"info":{"id":"msg_ours","role":"user","time":{"created":1}},"parts":[{"type":"text","text":"go"}]},` +
			`{"info":{"id":"msg_r","role":"assistant","time":{"created":2,"completed":3}},` +
			`"parts":[{"type":"tool","tool":"read","state":{"status":"error"}}]}]`))
	})
	ts := httptest.NewServer(h)
	defer ts.Close()
	srv := &OpenCodeServer{baseURL: ts.URL, authHeader: "Basic test", client: ts.Client()}

	prev := openCodeAsyncPollInterval
	openCodeAsyncPollInterval = time.Millisecond
	defer func() { openCodeAsyncPollInterval = prev }()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	start := time.Now()
	_, err := srv.waitForAsyncReply(ctx, "sess", "msg_ours")

	if err == nil {
		t.Fatal("a completed textless generation must be an error, not a silent success")
	}
	if ctx.Err() != nil {
		t.Fatalf("spun to the context deadline (%s) instead of failing fast — in production that is "+
			"a full 10-minute generateTimeout reported as `context deadline exceeded`: %v",
			time.Since(start), err)
	}
	if !strings.Contains(err.Error(), "without emitting any text") {
		t.Errorf("the error must name the actual outcome; got %v", err)
	}
	if polls > 3 {
		t.Errorf("took %d polls to notice a completed message; it is visible on the first one", polls)
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

// A SERIAL provider must not starve concurrent requests — this is the measured Windows failure.
//
// Reported from a real run: witness fired four simultaneous prompts at 21:11:01 and all four died at
// exactly 21:21:01 (the full 600s budget), then one solo retry three seconds later completed in 41s.
// Four such generations are ~164s of real work — they fit comfortably inside ONE request's budget, so
// concurrency was never too much work for the provider. The bug was that the deadline started when
// witness SENT, so a request waiting its turn was charged the same budget as one being generated, and
// the siblings at the back of the queue were killed having never been touched.
//
// The fake serve here IS that provider: it generates for exactly one request at a time and makes the
// others wait. With the old single-phase budget the waiters die; with the two-phase budget they wait
// and then succeed — which is what keeps concurrency worth having.
func TestConcurrentRequestsSurviveASeriallyServingProvider(t *testing.T) {
	var mu sync.Mutex
	// queue is the arrival order of request ids; only the head is "generating".
	var queue []string
	started := map[string]bool{}
	startTime := map[string]time.Time{}
	done := map[string]bool{}

	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.URL.Query().Get("id")
		mu.Lock()
		defer mu.Unlock()
		switch {
		case strings.Contains(r.URL.Path, "/prompt_async"):
			var body struct{ MessageID string }
			_ = json.NewDecoder(r.Body).Decode(&body)
			queue = append(queue, body.MessageID)
			w.WriteHeader(http.StatusNoContent)
		case strings.Contains(r.URL.Path, "/message"):
			// Serve strictly one at a time: the head of the queue starts, then completes; only
			// then does the next become eligible.
			if len(queue) > 0 {
				head := queue[0]
				if !started[head] {
					started[head] = true // generation begins for the head only
					startTime[head] = time.Now()
				} else if !done[head] && time.Since(startTime[head]) > 150*time.Millisecond {
					// The head takes real time to generate, so siblings genuinely accumulate
					// queue-wait. Without this the provider is effectively instant and no
					// starvation pressure exists — the fixture would prove nothing.
					done[head] = true
					queue = queue[1:]
				}
			}
			// Reply for whichever message the caller is polling about. The caller polls its own
			// session, so derive the id from the path.
			parts := strings.Split(r.URL.Path, "/")
			sess := ""
			if len(parts) > 2 {
				sess = parts[2]
			}
			msg := sessionMsg[sess]
			out := `[{"info":{"id":"` + msg + `","role":"user","time":{"created":1}},"parts":[{"type":"text","text":"go"}]}`
			if started[msg] {
				if done[msg] {
					out += `,{"info":{"id":"a_` + msg + `","role":"assistant","time":{"created":2,"completed":3}},` +
						`"parts":[{"type":"text","text":"[]"}]}`
				} else {
					// STARTED but not finished: an assistant message with no text yet. This is the
					// state the two-phase budget must treat as "generating", not "queued".
					out += `,{"info":{"id":"a_` + msg + `","role":"assistant","time":{"created":2}},"parts":[]}`
				}
			}
			out += `]`
			_, _ = w.Write([]byte(out))
		default:
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"id":"sess"}`))
		}
		_ = id
	})
	ts := httptest.NewServer(h)
	defer ts.Close()

	prev := openCodeAsyncPollInterval
	openCodeAsyncPollInterval = time.Millisecond
	defer func() { openCodeAsyncPollInterval = prev }()

	// Four concurrent waiters against the serial provider.
	srv := &OpenCodeServer{baseURL: ts.URL, authHeader: "Basic test", client: ts.Client()}
	var wg sync.WaitGroup
	errs := make([]error, 4)
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			msg := fmt.Sprintf("msg_%d", i)
			sess := fmt.Sprintf("s%d", i)
			mu.Lock()
			sessionMsg[sess] = msg
			mu.Unlock()
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			_, errs[i] = srv.runSessionWithMessage(ctx, sess, msg, "", "P", "I")
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("request %d failed against a serially-serving provider: %v\n"+
				"Concurrency must survive a provider that serves one at a time — killing the "+
				"waiters is the measured Windows starvation this budget exists to prevent.", i, err)
		}
	}
}

// sessionMsg maps a fake session id to the message id under test, so the handler can answer each
// caller about its own request.
var sessionMsg = map[string]string{}

// The GENERATION deadline must still bite once generation has actually started — the two-phase
// budget must not become "wait forever". This is the half the fix could break.
func TestGenerationDeadlineStillAppliesOnceStarted(t *testing.T) {
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Always: our request, plus a STARTED assistant message that never completes.
		_, _ = w.Write([]byte(`[{"info":{"id":"msg_ours","role":"user","time":{"created":1}},"parts":[{"type":"text","text":"go"}]},` +
			`{"info":{"id":"a","role":"assistant","time":{"created":2}},"parts":[]}]`))
	})
	ts := httptest.NewServer(h)
	defer ts.Close()
	srv := &OpenCodeServer{baseURL: ts.URL, authHeader: "Basic test", client: ts.Client()}

	prevPoll := openCodeAsyncPollInterval
	openCodeAsyncPollInterval = time.Millisecond
	defer func() { openCodeAsyncPollInterval = prevPoll }()

	// A ctx shorter than the generation budget stands in for it: the point is that a STARTED
	// generation which never completes still terminates, rather than waiting the queue allowance.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	start := time.Now()
	if _, err := srv.waitForAsyncReply(ctx, "sess", "msg_ours"); err == nil {
		t.Fatal("a generation that starts and never completes must eventually fail")
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Errorf("took %s to give up on a started-but-never-completing generation", elapsed)
	}
}
