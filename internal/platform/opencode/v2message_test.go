package opencode

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
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

func TestParseTurnStartsWhenRetryClears(t *testing.T) {
	// Read the retrying fixture
	data := readFixture(t, "v2_turn_retrying.json")

	// First: verify the retrying fixture shows Started=false
	st, err := parseTurn(data, "msg_req")
	if err != nil {
		t.Fatalf("parseTurn with retry: %v", err)
	}
	if st.Started {
		t.Error("Started = true when retry is present, want false")
	}
	if st.Retry == nil {
		t.Error("Retry = nil when assistant has retry field")
	}

	// Second: clear retry and verify Started=true
	var response struct {
		Data []v2Message `json:"data"`
	}
	if err := json.Unmarshal(data, &response); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	// Find the assistant and clear its retry
	for i := range response.Data {
		if response.Data[i].Type == "assistant" {
			response.Data[i].Retry = nil
			// Add content so there's something to parse
			response.Data[i].Content = []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			}{
				{Type: "text", Text: "reply after retry cleared"},
			}
		}
	}
	clearedData, err := json.Marshal(response)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	st2, err := parseTurn(clearedData, "msg_req")
	if err != nil {
		t.Fatalf("parseTurn without retry: %v", err)
	}
	if !st2.Started {
		t.Error("Started = false when retry is cleared, want true")
	}
	if st2.Retry != nil {
		t.Errorf("Retry = %v when assistant has no retry, want nil", st2.Retry)
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

func TestFreeLimitIsDetectedFromTheGatewayBody(t *testing.T) {
	// Test that type "provider.rate-limit" but response.body contains "FreeUsageLimitError"
	// becomes QuotaExhausted
	e := v2Error{
		Type:    "provider.rate-limit",
		Message: "Rate limit exceeded",
		Status:  429,
	}
	e.Response = &struct {
		Body string `json:"body"`
	}{
		Body: `{"type":"error","error":{"type":"FreeUsageLimitError","message":"Rate limit exceeded"}}`,
	}
	if !isFreeLimit(e) {
		t.Error("isFreeLimit = false for gateway body with FreeUsageLimitError")
	}

	// Verify it produces QuotaExhaustedError
	idle := v2Message{
		Outcome: "failed",
		Error:   &e,
	}
	idle.Time.Created = 1763046000000
	st := turnState{
		Idle: &idle,
	}
	var qe *platform.QuotaExhaustedError
	err := turnError(st, time.Now())
	if !errors.As(err, &qe) {
		t.Errorf("turnError did not return QuotaExhaustedError for gateway body: %v", err)
	}
}

func TestPlainRateLimitIsNotFreeLimit(t *testing.T) {
	// A provider.rate-limit error (429) without FreeUsageLimitError in the body
	// should NOT become QuotaExhaustedError
	plainRateLimit := v2Error{
		Type:    "provider.rate-limit",
		Message: "Rate limit exceeded. Please try again later.",
		Status:  429,
	}
	plainRateLimit.Response = &struct {
		Body string `json:"body"`
	}{
		Body: `{"type":"error","error":{"type":"RateLimitError","message":"Rate limit exceeded"}}`,
	}

	// Test on idle error
	idle := v2Message{
		Outcome: "failed",
		Error:   &plainRateLimit,
	}
	idle.Time.Created = 1763046000000
	st := turnState{
		Idle: &idle,
	}

	var qe *platform.QuotaExhaustedError
	err := turnError(st, time.Now())
	if err == nil {
		t.Fatal("turnError returned nil for rate-limit error")
	}
	if errors.As(err, &qe) {
		t.Errorf("turnError returned QuotaExhaustedError for plain rate-limit: %v", err)
	}
	if !containsString(err.Error(), "rate-limit") {
		t.Errorf("error message does not contain 'rate-limit': %v", err)
	}

	// Test on assistant error (when idle has no error)
	idle2 := v2Message{
		Outcome: "failed",
	}
	idle2.Time.Created = 1763046000000
	last := v2Message{
		Error: &plainRateLimit,
	}
	last.Time.Created = 1763046000000
	st2 := turnState{
		Idle: &idle2,
		Last: &last,
	}

	err2 := turnError(st2, time.Now())
	if err2 == nil {
		t.Fatal("turnError returned nil for rate-limit error on Last")
	}
	if errors.As(err2, &qe) {
		t.Errorf("turnError returned QuotaExhaustedError for plain rate-limit on Last: %v", err2)
	}
	if !containsString(err2.Error(), "rate-limit") {
		t.Errorf("error message does not contain 'rate-limit': %v", err2)
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

func TestNoRouteIsModelUnavailable(t *testing.T) {
	data := readFixture(t, "v2_turn_no_route.json")
	st, err := parseTurn(data, "msg_req")
	if err != nil {
		t.Fatalf("parseTurn: %v", err)
	}

	err = turnError(st, time.Now())
	if !errors.Is(err, errOpenCodeModelUnavailable) {
		t.Errorf("error is not errOpenCodeModelUnavailable: %v", err)
	}
	if err == nil || !containsString(err.Error(), "Model unavailable") {
		t.Errorf("error message does not contain 'Model unavailable': %v", err)
	}
}

func TestInterruptedTurnIsAnError(t *testing.T) {
	idle := v2Message{
		Outcome: "interrupted",
	}
	idle.Time.Created = time.Now().UnixMilli()
	st := turnState{
		Idle: &idle,
	}
	err := turnError(st, time.Now())
	if err == nil {
		t.Fatal("turnError returned nil for interrupted turn")
	}
	if !containsString(err.Error(), "interrupted") {
		t.Errorf("error does not contain 'interrupted': %v", err)
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

func containsString(s, substr string) bool {
	return len(s) > 0 && len(substr) > 0 && (s == substr || len(s) > len(substr) && (s[:len(substr)] == substr || s[len(s)-len(substr):] == substr || contains(s, substr)))
}

func contains(s, substr string) bool {
	for i := 0; i+len(substr) <= len(s); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
