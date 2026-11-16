//go:build opencodecontract

package opencode

// What witness reads from a REAL OpenCode v2, on the harness's mock provider (no model calls):
//   - TestContractPluginCapture: after `witness wire opencode`, a serve on that config loads the
//     wired plugin and connects mcp.witness, and mock turns reach witness's L0 through the
//     plugin's turn-end import;
//   - TestContractSessionErrorShapes: the turn shapes the runner acts on, read through the
//     runner's own poll page and parseTurn/turnError (v2message.go), so a field v2 renames fails
//     here instead of reading as zero in production.

import (
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/IngTian/witness/internal/platform"
	"github.com/IngTian/witness/internal/store"
)

func TestContractPluginCapture(t *testing.T) {
	oc := requireContract(t)
	noLeaks(t)
	s := scratchEnv(t)
	s.setProcessEnv(t) // store.Open below opens the scratch archive
	m := startMock(t)
	disableAutoDistill(t, s)                  // before any witness runs: no stray auto worker
	runWitness(t, s, nil, "wire", "opencode") // Windows: copies into the scratch LOCALAPPDATA and edits this runner's user PATH
	srv := startRawServe(t, oc.Bin, s, "OPENCODE_CONFIG_CONTENT="+m.ProviderConfig())

	// What the last poll of each wait saw, for a failure (waitFor says only what timed out).
	var plugins, mcp []byte
	var recs []store.RawRecord
	var readErr error
	defer func() {
		if t.Failed() {
			t.Logf("last GET /api/plugin: %.2000s\nlast GET /api/mcp: %.1000s\nlast L0 read: %+v (err %v)", plugins, mcp, recs, readErr)
		}
	}()

	type plugin struct {
		ID    string
		State struct{ Status, Error string }
	}
	waitFor(t, 20*time.Second, "witness plugin active", func() bool {
		_, plugins = srv.Do(http.MethodGet, atWork("/api/plugin", s), nil, true)
		var r struct{ Data []plugin }
		_ = json.Unmarshal(plugins, &r)
		return slices.ContainsFunc(r.Data, func(p plugin) bool { return p.ID == "witness" && p.State.Status == "active" })
	})
	// Listed is only the merged config entry; connected is `witness mcp`, started through the
	// wired target, answering the MCP handshake (pending first, for a second or two).
	type mcpServer struct {
		Name   string
		Status struct{ Status string }
	}
	waitFor(t, 30*time.Second, "mcp.witness connected", func() bool {
		_, mcp = srv.Do(http.MethodGet, atWork("/api/mcp", s), nil, true)
		var r struct{ Data []mcpServer }
		_ = json.Unmarshal(mcp, &r)
		return slices.ContainsFunc(r.Data, func(m mcpServer) bool { return m.Name == "witness" && m.Status.Status == "connected" })
	})

	// Two turns: the plugin's startup sync (once per serve) may race the first into L0; only its
	// turn-end import can bring the second, a new session prompted once the first is in.
	for i := range 2 {
		marker := "CONTRACT-USER-" + strconv.Itoa(i) + "-" + strconv.FormatInt(time.Now().UnixNano(), 36)
		sid := srv.Prompt(t, "ok", marker)
		st, page := waitTurn(t, srv, sid, 30*time.Second, "idle", func(st turnState) bool { return st.Idle != nil })
		if st.Idle.Outcome != "succeeded" {
			t.Fatalf("mock turn %d did not succeed: %.2000s", i, page)
		}
		waitFor(t, 30*time.Second, "turn "+strconv.Itoa(i)+" in L0", func() bool {
			st, err := store.Open()
			if readErr = err; err != nil {
				return false
			}
			defer st.Close()
			recs, readErr = st.ReadRaw(SessionPrefix + sid)
			return slices.ContainsFunc(recs, func(r store.RawRecord) bool { return r.Role == "user" && strings.Contains(r.Text, marker) }) &&
				slices.ContainsFunc(recs, func(r store.RawRecord) bool {
					return r.Role == "assistant" && strings.Contains(r.Text, "MOCK-REPLY")
				})
		})
	}
}

func TestContractSessionErrorShapes(t *testing.T) {
	oc := requireContract(t)
	noLeaks(t)
	s := scratchEnv(t)
	m := startMock(t)
	srv := startRawServe(t, oc.Bin, s, "OPENCODE_CONFIG_CONTENT="+m.ProviderConfig())
	idle := func(st turnState) bool { return st.Idle != nil }
	var quota *platform.QuotaExhaustedError

	t.Run("free-limit", func(t *testing.T) { // terminal at once: the drain pauses until the reset
		sid := srv.Prompt(t, "free", "x")
		st, page := waitTurn(t, srv, sid, 20*time.Second, "idle", idle)
		if st.Last == nil || st.Last.Error == nil || st.Last.Time.Completed == nil || st.Idle.Error == nil {
			t.Fatalf("want an assistant with error and time.completed, and an idle with error: %.2000s", page)
		}
		if e := st.Last.Error; e.Type != "provider.free-limit" || e.Status != 429 || e.RetryAfterMs == nil || *e.RetryAfterMs != 3_600_000 {
			t.Fatalf("assistant error: want provider.free-limit, status 429, retryAfterMs 3600000: %.2000s", page)
		}
		if st.Idle.Outcome != "failed" || st.Idle.Error.Type != "provider.free-limit" {
			t.Fatalf("idle: want outcome failed with error provider.free-limit: %.2000s", page)
		}
		// The mock's retry-after (1 h) equals turnError's fallback, so the retryAfterMs read is
		// pinned by the check above; this one pins the base (time.completed) and the type.
		err := turnError(st, time.Now())
		if !errors.As(err, &quota) || !quota.ResetAt.Equal(time.UnixMilli(*st.Last.Time.Completed).Add(time.Hour)) {
			t.Fatalf("turnError = %v, want a QuotaExhaustedError resetting at time.completed + retryAfterMs", err)
		}
		if n := len(m.Calls("free")); n != 1 {
			t.Fatalf("the mock saw %d free calls, want 1 (OpenCode never retries the free limit)", n)
		}
	})

	t.Run("rate-limit-exhausted", func(t *testing.T) {
		sid := srv.Prompt(t, "exhausted", "x")
		if oc.Version.Major == 2 && oc.Version.Minor < 3 {
			// 2.2.7 reads "rate limit exhausted" as a terminal quota (#59816 fixed that in 2.3.0).
			st, page := waitTurn(t, srv, sid, 10*time.Second, "idle", idle)
			if st.Last == nil || st.Last.Error == nil || st.Last.Error.Type != "provider.quota" || st.Idle.Outcome != "failed" {
				t.Fatalf("want an assistant error provider.quota and an idle outcome failed: %.2000s", page)
			}
			if err := turnError(st, time.Now()); err == nil || errors.As(err, &quota) {
				t.Fatalf("turnError = %v, want a plain failure: provider.quota is not the free limit and must not pause the drain", err)
			}
			if n := len(m.Calls("exhausted")); n != 1 {
				t.Fatalf("the mock saw %d exhausted calls, want 1 (terminal on 2.2.7)", n)
			}
			return
		}
		// 2.3+: retried, the backoff showing as assistant.retry on a turn not yet started (parseTurn
		// sets Retry only then).
		waitTurn(t, srv, sid, 5*time.Second, "assistant.retry with provider.rate-limit", func(st turnState) bool {
			return st.Retry != nil && st.Retry.Error.Type == "provider.rate-limit"
		})
		code, b := srv.Do(http.MethodPost, "/api/session/"+sid+"/interrupt", nil, true)
		var r struct{ Interrupted bool }
		if code != http.StatusOK || json.Unmarshal(b, &r) != nil || !r.Interrupted {
			t.Fatalf("POST /api/session/%s/interrupt: %d %.500s, want 200 {\"interrupted\":true}", sid, code, b)
		}
		st, page := waitTurn(t, srv, sid, 5*time.Second, "idle", idle)
		if st.Idle.Outcome != "interrupted" {
			t.Fatalf("idle: want outcome interrupted: %.2000s", page)
		}
		if err := turnError(st, time.Now()); err == nil || !strings.Contains(err.Error(), "interrupted") {
			t.Fatalf("turnError = %v, want the interrupted error", err)
		}
		calls := len(m.Calls("exhausted"))
		time.Sleep(5 * time.Second) // the retry-after is 2 s: a retry loop that outlived the interrupt shows here
		if n := len(m.Calls("exhausted")); n != calls {
			t.Fatalf("the mock saw %d more exhausted calls in the 5 s after the interrupt", n-calls)
		}
	})

	t.Run("retry-clears-at-first-chunk", func(t *testing.T) { // the #143 "generation started" predicate
		sid := srv.Prompt(t, "flaky", "x")
		var retry *v2Retry
		var retryIn time.Duration // retry.at - now, when first seen
		st, page := waitTurn(t, srv, sid, 20*time.Second, "idle", func(st turnState) bool {
			if retry == nil && st.Retry != nil { // set only while not started: the backoff is queue time
				retry, retryIn = st.Retry, time.Until(time.UnixMilli(st.Retry.At))
			}
			return st.Idle != nil
		})
		// retry.at in epoch ms is what the runner's early give-up compares with its queue budget.
		if retry == nil || retry.Error.Type != "provider.rate-limit" || retryIn.Abs() > 30*time.Second {
			t.Fatalf("want a snapshot with assistant.retry (provider.rate-limit, at within 30 s of now): retry %+v, at-now %s; final page %.2000s", retry, retryIn, page)
		}
		if !st.Started || st.Last.Retry != nil || st.Idle.Outcome != "succeeded" || !strings.Contains(st.Reply, "MOCK-REPLY") {
			t.Fatalf("final: want started, retry cleared, idle succeeded, reply MOCK-REPLY: %.2000s", page)
		}
		if err := turnError(st, time.Now()); err != nil {
			t.Fatalf("turnError = %v, want nil", err)
		}
		if n := len(m.Calls("flaky")); n != 2 {
			t.Fatalf("the mock saw %d flaky calls, want 2 (one 429, one success)", n)
		}
	})
}

// waitTurn reads sid's turn as the runner does, every 200 ms until ok or d: its poll page
// (messagesPath) through parseTurn, keyed on the prompt's user record (Prompt sends no id, so the
// serve's stands in for the runner's). It returns the page too, for failure messages.
func waitTurn(t *testing.T, srv *rawServe, sid string, d time.Duration, what string, ok func(turnState) bool) (turnState, []byte) {
	t.Helper()
	for deadline := time.Now().Add(d); ; time.Sleep(200 * time.Millisecond) {
		code, b := srv.Do(http.MethodGet, messagesPath(sid), nil, true)
		var page struct{ Data []v2Message }
		if code != http.StatusOK || json.Unmarshal(b, &page) != nil {
			t.Fatalf("GET %s: %d %.500s", messagesPath(sid), code, b)
		}
		if i := slices.IndexFunc(page.Data, func(m v2Message) bool { return m.Type == "user" }); i >= 0 {
			if st, _ := parseTurn(b, page.Data[i].ID); ok(st) { // its only error is the unmarshal above
				return st, b
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("session %s: no %s after %s; last page: %.2000s", sid, what, d, b)
		}
	}
}
