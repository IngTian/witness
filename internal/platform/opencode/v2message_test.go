package opencode

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/IngTian/witness/internal/platform"
)

func TestQuotaExhaustedErrorNamesTheReset(t *testing.T) {
	e := &platform.QuotaExhaustedError{
		ResetAt: time.Date(2026, 11, 13, 15, 0, 0, 0, time.UTC),
		Runtime: "opencode",
	}
	want := "opencode free usage limit reached; resets at 2026-11-13T15:00:00Z"
	if got := e.Error(); got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
}

func TestParseTurnReadsTheSucceededShape(t *testing.T) {
	data := readFixture(t, "v2_turn_succeeded.json")
	st, err := parseTurn(data, "msg_req")
	if err != nil {
		t.Fatalf("parseTurn: %v", err)
	}
	if !st.Present {
		t.Error("Present = false, want true")
	}
	if !st.Started {
		t.Error("Started = false, want true")
	}
	if st.Idle == nil || st.Idle.Outcome != "succeeded" {
		t.Errorf("Idle.Outcome = %v, want succeeded", st.Idle)
	}
	if st.Reply != "[]" {
		t.Errorf("Reply = %q, want %q", st.Reply, "[]")
	}
	if err := turnError(st, time.Now()); err != nil {
		t.Errorf("turnError = %v, want nil", err)
	}
}

func TestParseTurnCountsRetryAsQueued(t *testing.T) {
	data := readFixture(t, "v2_turn_retrying.json")
	st, err := parseTurn(data, "msg_req")
	if err != nil {
		t.Fatalf("parseTurn: %v", err)
	}
	if st.Started {
		t.Error("Started = true, want false")
	}
	if st.Retry == nil {
		t.Fatal("Retry = nil")
	}
	if st.Retry.Attempt != 2 {
		t.Errorf("Retry.Attempt = %d, want 2", st.Retry.Attempt)
	}
	if st.Retry.Error.Type != "provider.rate-limit" {
		t.Errorf("Retry.Error.Type = %q, want provider.rate-limit", st.Retry.Error.Type)
	}
}

// The retrying fixture's turn once its retry clears: the same assistant, with no retry field.
func TestParseTurnStartsWhenRetryClears(t *testing.T) {
	data := []byte(`{"data":[
		{"id":"msg_req","type":"user","time":{"created":1763046000000}},
		{"id":"msg_asst","type":"assistant","time":{"created":1763046001000},
		 "content":[{"type":"text","text":"reply after retry cleared"}]}]}`)
	st, err := parseTurn(data, "msg_req")
	if err != nil {
		t.Fatalf("parseTurn: %v", err)
	}
	if !st.Started || st.Retry != nil {
		t.Errorf("Started = %v, Retry = %v; want started, with no retry", st.Started, st.Retry)
	}
}

func TestFreeLimitBecomesQuotaExhausted(t *testing.T) {
	data := readFixture(t, "v2_turn_free_limit.json")
	st, err := parseTurn(data, "msg_req")
	if err != nil {
		t.Fatalf("parseTurn: %v", err)
	}

	var qe *platform.QuotaExhaustedError
	err = turnError(st, time.Now())
	if !errors.As(err, &qe) {
		t.Fatalf("turnError did not return QuotaExhaustedError: %v", err)
	}

	// Fixture has completed=1763046000000, retryAfterMs=3600000
	wantReset := time.UnixMilli(1763046000000 + 3600000)
	if !qe.ResetAt.Equal(wantReset) {
		t.Errorf("ResetAt = %v, want %v", qe.ResetAt, wantReset)
	}
	if qe.Runtime != "opencode" {
		t.Errorf("Runtime = %q, want opencode", qe.Runtime)
	}
}

// turnError's verdict on each failed-turn shape. The plain rate limit is checked on the idle and,
// when the idle carries no error, on the last assistant (the idle→last fallback).
func TestTurnErrorClassifiesFailedTurns(t *testing.T) {
	noRoute, err := parseTurn(readFixture(t, "v2_turn_no_route.json"), "msg_req")
	if err != nil {
		t.Fatalf("parseTurn: %v", err)
	}
	rateLimit := func(body string) *v2Error {
		e := &v2Error{Type: "provider.rate-limit", Message: "Rate limit exceeded", Status: 429}
		e.Response = &struct {
			Body string `json:"body"`
		}{body}
		return e
	}
	gateway := rateLimit(`{"type":"error","error":{"type":"FreeUsageLimitError","message":"Rate limit exceeded"}}`)
	plain := rateLimit(`{"type":"error","error":{"type":"RateLimitError","message":"Rate limit exceeded"}}`)
	for _, tc := range []struct {
		name       string
		st         turnState
		wantQuota  bool
		wantIs     error
		wantSubstr string
	}{
		{"free limit in the gateway body", turnState{Idle: &v2Message{Outcome: "failed", Error: gateway}}, true, nil, ""},
		{"plain rate limit on the idle", turnState{Idle: &v2Message{Outcome: "failed", Error: plain}}, false, nil, "rate-limit"},
		{"plain rate limit on the last assistant", turnState{Idle: &v2Message{Outcome: "failed"}, Last: &v2Message{Error: plain}}, false, nil, "rate-limit"},
		{"no route", noRoute, false, errOpenCodeModelUnavailable, "Model unavailable"},
		{"interrupted", turnState{Idle: &v2Message{Outcome: "interrupted"}}, false, nil, "interrupted"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := turnError(tc.st, time.Now())
			if err == nil {
				t.Fatal("turnError = nil, want an error")
			}
			var qe *platform.QuotaExhaustedError
			if got := errors.As(err, &qe); got != tc.wantQuota {
				t.Errorf("QuotaExhaustedError = %v, want %v: %v", got, tc.wantQuota, err)
			}
			if tc.wantIs != nil && !errors.Is(err, tc.wantIs) {
				t.Errorf("error is not %v: %v", tc.wantIs, err)
			}
			if !strings.Contains(err.Error(), tc.wantSubstr) {
				t.Errorf("error %q does not contain %q", err, tc.wantSubstr)
			}
		})
	}
}

func TestFreeLimitResetFallsBackToOneHour(t *testing.T) {
	now := time.Now()
	const hour = time.Hour

	// retryAfterMs absent → completed+1h
	last := v2Message{}
	last.Time.Created = now.Add(-10 * time.Minute).UnixMilli()
	last.Time.Completed = ptr(now.Add(-5 * time.Minute).UnixMilli())
	st := turnState{
		Last: &last,
	}
	e := v2Error{Type: "provider.free-limit"}
	reset := freeLimitResetAt(st, e, now)
	want := time.UnixMilli(*st.Last.Time.Completed).Add(hour)
	if !reset.Equal(want) {
		t.Errorf("retryAfterMs absent: got %v, want %v", reset, want)
	}

	// retryAfterMs = 46 days → completed+1h
	bigDelay := int64(46 * 24 * time.Hour / time.Millisecond)
	e.RetryAfterMs = &bigDelay
	reset = freeLimitResetAt(st, e, now)
	if !reset.Equal(want) {
		t.Errorf("retryAfterMs=46days: got %v, want %v", reset, want)
	}

	// no completed → idle.time.created+retryAfterMs
	delay := int64(2 * time.Hour / time.Millisecond)
	e.RetryAfterMs = &delay
	st.Last.Time.Completed = nil
	idle := v2Message{}
	idle.Time.Created = now.Add(-1 * time.Hour).UnixMilli()
	st.Idle = &idle
	reset = freeLimitResetAt(st, e, now)
	want = time.UnixMilli(st.Idle.Time.Created).Add(2 * time.Hour)
	if !reset.Equal(want) {
		t.Errorf("no completed, use idle: got %v, want %v", reset, want)
	}

	// neither → now+retryAfterMs
	st.Idle = nil
	st.Last = nil
	reset = freeLimitResetAt(st, e, now)
	// Allow 1 second tolerance for test execution time
	if reset.Sub(now.Add(2*time.Hour)).Abs() > time.Second {
		t.Errorf("neither completed nor idle: got %v, want ~%v", reset, now.Add(2*time.Hour))
	}
}

func TestParseTurnIgnoresRecordsBeforeOurRequest(t *testing.T) {
	data := readFixture(t, "v2_turn_history.json")
	st, err := parseTurn(data, "msg_req")
	if err != nil {
		t.Fatalf("parseTurn: %v", err)
	}

	// An earlier failed idle should be ignored
	if st.Idle != nil {
		t.Errorf("Idle should be nil (earlier failed idle ignored), got %v", st.Idle.Outcome)
	}
	if err := turnError(st, time.Now()); err != nil {
		t.Errorf("turnError = %v, want nil (earlier error ignored)", err)
	}
}

func TestParseTurnWithoutOurRequestIsNotPresent(t *testing.T) {
	data := readFixture(t, "v2_turn_succeeded.json")
	st, err := parseTurn(data, "msg_absent")
	if err != nil {
		t.Fatalf("parseTurn: %v", err)
	}
	if st.Present {
		t.Error("Present = true for absent requestID")
	}
}

func TestDeadTurnHasNoIdle(t *testing.T) {
	data := readFixture(t, "v2_turn_dead.json")
	st, err := parseTurn(data, "msg_req")
	if err != nil {
		t.Fatalf("parseTurn: %v", err)
	}
	if st.Idle != nil {
		t.Errorf("Idle = %v, want nil", st.Idle)
	}
	if st.Reply != "" {
		t.Errorf("Reply = %q, want empty", st.Reply)
	}
}

// Helper functions

func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	path := filepath.Join("testdata", name)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("readFixture %s: %v", name, err)
	}
	return data
}

func ptr[T any](v T) *T {
	return &v
}
