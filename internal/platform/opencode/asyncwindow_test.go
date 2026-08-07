package opencode

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
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
	return string(b)
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
