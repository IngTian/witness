package opencode

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestValidateOpenCodeCapability(t *testing.T) {
	old := openCodeVersionCommand
	defer func() { openCodeVersionCommand = old }()

	// Supported version passes
	openCodeVersionCommand = func(context.Context, string) ([]byte, error) { return []byte("2.2.7\n"), nil }
	if err := ValidateOpenCodeCapability(context.Background(), ""); err != nil {
		t.Fatalf("2.2.7 must be accepted, got %v", err)
	}

	// Below MinVersion is refused
	openCodeVersionCommand = func(context.Context, string) ([]byte, error) { return []byte("2.2.6\n"), nil }
	err := ValidateOpenCodeCapability(context.Background(), "")
	if !errors.Is(err, ErrOpenCodeTooOld) {
		t.Fatalf("2.2.6 error = %v, want ErrOpenCodeTooOld", err)
	}
	if strings.Contains(err.Error(), "deadline") {
		t.Errorf("an old version was rejected by TIMEOUT instead of by version check: %v", err)
	}

	// v1 is refused by name
	openCodeVersionCommand = func(context.Context, string) ([]byte, error) { return []byte("1.19.5\n"), nil }
	err = ValidateOpenCodeCapability(context.Background(), "")
	if !errors.Is(err, ErrOpenCodeV1) {
		t.Fatalf("1.19.5 error = %v, want ErrOpenCodeV1", err)
	}
}

func TestOpenCodeServerRunCreatesSendsAndDeletesSession(t *testing.T) {
	var created, prompted, deleted bool
	var requestMessageID string
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Basic test" {
			t.Fatalf("auth header = %q", got)
		}
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/session":
			created = true
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			if body["title"] != "witness" || body["agent"] != "witness" {
				t.Fatalf("bad session body: %#v", body)
			}
			model := body["model"].(map[string]any)
			if model["providerID"] != "openai" || model["id"] != "gpt-5.5" {
				t.Fatalf("bad session model: %#v", model)
			}
			_, _ = w.Write([]byte(`{"id":"ses_test"}`))
		case r.Method == http.MethodPost && r.URL.Path == "/session/ses_test/prompt_async":
			prompted = true
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			if !strings.HasPrefix(body["messageID"].(string), "msg_") {
				t.Fatalf("messageID not generated: %#v", body["messageID"])
			}
			requestMessageID = body["messageID"].(string)
			if body["agent"] != "witness" {
				t.Fatalf("bad agent: %#v", body["agent"])
			}
			// The trusted/untrusted split: instructions on `system`, corpus as the text part, each
			// VERBATIM — distill.RunnerMine has already fenced both, so the runner must not again.
			if system := body["system"].(string); system != "EXTRACT" {
				t.Fatalf("system prompt not passed verbatim: %q", system)
			}
			parts := body["parts"].([]any)
			part := parts[0].(map[string]any)
			if part["type"] != "text" || part["text"] != "DATA" {
				t.Fatalf("bad parts: %#v", body["parts"])
			}
			model := body["model"].(map[string]any)
			if model["providerID"] != "openai" || model["modelID"] != "gpt-5.5" {
				t.Fatalf("bad message model: %#v", model)
			}
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodGet && r.URL.Path == "/session/ses_test/message":
			_, _ = fmt.Fprintf(w, `[
				{"info":{"id":%q,"role":"user"},"parts":[{"id":"prt_u","type":"text","text":"DATA"}]},
				{"info":{"id":"msg_reply","role":"assistant"},"parts":[{"id":"prt_1","type":"text","text":"RESULT"}]}
			]`, requestMessageID)
		case r.Method == http.MethodDelete && r.URL.Path == "/session/ses_test":
			deleted = true
			_, _ = w.Write([]byte(`{}`))
		default:
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	})
	ts := httptest.NewServer(h)
	defer ts.Close()
	srv := &OpenCodeServer{baseURL: ts.URL, authHeader: "Basic test", client: ts.Client()}

	got, err := srv.Run(context.Background(), "openai/gpt-5.5", "EXTRACT", "DATA")
	if err != nil {
		t.Fatal(err)
	}
	if got != "RESULT" {
		t.Fatalf("got %q", got)
	}
	if !created || !prompted || !deleted {
		t.Fatalf("created=%v prompted=%v deleted=%v", created, prompted, deleted)
	}
}

func TestOpenCodeServerRunAbortsOnAsyncTimeout(t *testing.T) {
	// Deterministic timeout: a large poll interval means the ticker never races
	// ctx.Done(), and the caller's context is cancelled from INSIDE the first
	// /message poll (below) — so the deadline can only fire once the poll loop is
	// genuinely reached. The old version used a 5ms wall-clock deadline that raced
	// createSession/prompt_async: if it fired during setup, Run bailed before the
	// poll loop and abort/delete never ran, flaking CI as aborted=false.
	oldPoll := openCodeAsyncPollInterval
	openCodeAsyncPollInterval = time.Hour
	defer func() { openCodeAsyncPollInterval = oldPoll }()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var aborted, deleted bool
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/session":
			_, _ = w.Write([]byte(`{"id":"ses_timeout"}`))
		case r.Method == http.MethodPost && r.URL.Path == "/session/ses_timeout/prompt_async":
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodGet && r.URL.Path == "/session/ses_timeout/message":
			// The reply never arrives; cancel the caller's context so waitForAsyncReply
			// hits ctx.Done() on the very next select. abort/delete use their own
			// background contexts, so this cancellation cannot suppress them.
			_, _ = w.Write([]byte(`[]`))
			cancel()
		case r.Method == http.MethodPost && r.URL.Path == "/session/ses_timeout/abort":
			aborted = true
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodDelete && r.URL.Path == "/session/ses_timeout":
			deleted = true
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	})
	ts := httptest.NewServer(h)
	defer ts.Close()
	srv := &OpenCodeServer{baseURL: ts.URL, authHeader: "Basic test", client: ts.Client()}

	if _, err := srv.Run(ctx, "", "EXTRACT", "DATA"); err == nil {
		t.Fatal("Run should time out")
	}
	if !aborted || !deleted {
		t.Fatalf("aborted=%v deleted=%v", aborted, deleted)
	}
}

func TestParseOpenCodeMessageResponse(t *testing.T) {
	data := []byte(`{"parts":[{"id":"p1","type":"text","text":"first"},{"id":"p2","type":"reasoning","text":"hidden"},{"id":"p2","type":"text","text":"second"}]}`)
	if got := parseOpenCodeMessageResponse(data); got != "first\n\nsecond" {
		t.Fatalf("got %q", got)
	}
}

func TestParseOpenCodeAsyncReplySkipsRequestMessage(t *testing.T) {
	data := []byte(`[
		{"info":{"id":"msg_old","role":"assistant"},"parts":[{"id":"p0","type":"text","text":"historical"}]},
		{"info":{"id":"msg_request","role":"user"},"parts":[{"id":"p1","type":"text","text":"input"}]},
		{"info":{"id":"msg_reply","role":"assistant"},"parts":[{"id":"p2","type":"text","text":"output"}]}
	]`)
	if got := parseOpenCodeAsyncReply(data, "msg_request"); got != "output" {
		t.Fatalf("got %q", got)
	}
	if got := parseOpenCodeAsyncReply(data, "msg_missing"); got != "" {
		t.Fatalf("historical reply leaked into unmatched request: %q", got)
	}
}

func TestParseOpenCodeModels(t *testing.T) {
	out := strings.Join([]string{
		"openai/gpt-5.5",
		"openai/gpt-5.5-fast",
		"openai/gpt-5.5",
		"metadata without model",
	}, "\n")
	got := parseOpenCodeModels(out)
	want := []string{"openai/gpt-5.5", "openai/gpt-5.5-fast"}
	if !slices.Equal(got, want) {
		t.Fatalf("got %#v, want %#v", got, want)
	}
}

func TestModelHint(t *testing.T) {
	if got := modelHint(nil); !strings.Contains(got, "returned no models") {
		t.Fatalf("empty hint = %q", got)
	}
	if got := modelHint([]string{"openai/gpt-5.5"}); !strings.Contains(got, "openai/gpt-5.5") {
		t.Fatalf("model hint = %q", got)
	}
}

// A generation that FAILED at the provider is not an empty response: OpenCode marks the
// assistant message completed, attaches info.error, and emits ZERO text parts. Without
// checking info.error that is indistinguishable from "still generating", so the poll used
// to spin for the full 10-minute generateTimeout and then blame a timeout — burying the
// real, actionable cause.
//
// testdata/async_provider_error.json is a VERBATIM capture from real OpenCode 1.18.3
// (an unfunded account: APIError / 401 / "Insufficient balance"), workspace id redacted.
func TestAsyncProviderErrorIsSurfacedNotPolled(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("testdata", "async_provider_error.json"))
	if err != nil {
		t.Fatal(err)
	}
	const requestID = "msg_wreq1785880955"

	// The reply parser still (correctly) sees no text — that is why the error check exists.
	if reply := parseOpenCodeAsyncReply(data, requestID); reply != "" {
		t.Fatalf("no text parts exist, so the reply must be empty, got %q", reply)
	}
	// The error parser must extract the provider's own message.
	got := parseOpenCodeAsyncError(data, requestID)
	if got == "" {
		t.Fatal("a completed-with-error assistant message must surface an error, got none")
	}
	for _, want := range []string{"APIError", "401", "Insufficient balance"} {
		if !strings.Contains(got, want) {
			t.Errorf("error %q should mention %q so the user can act on it", got, want)
		}
	}
}

// The error must be attributed to THIS request only. A native fork carries the source
// conversation, so a failure already present in that history must never be reported as
// this run's outcome — and nothing at all until our own request appears in the window.
func TestAsyncProviderErrorIgnoresHistoryAndPreRequest(t *testing.T) {
	errMsg := `{"info":{"id":"msg_old","role":"assistant","time":{"completed":1},` +
		`"error":{"name":"APIError","data":{"message":"an OLD failure from the forked history","statusCode":500}}},"parts":[]}`
	req := `{"info":{"id":"msg_ours","role":"user"},"parts":[{"type":"text","text":"go"}]}`

	// Error BEFORE our request → not ours.
	if got := parseOpenCodeAsyncError([]byte("["+errMsg+","+req+"]"), "msg_ours"); got != "" {
		t.Errorf("a failure preceding our request must be ignored, got %q", got)
	}
	// Our request absent entirely → nothing is attributable yet.
	if got := parseOpenCodeAsyncError([]byte("["+errMsg+"]"), "msg_ours"); got != "" {
		t.Errorf("with our request absent nothing is ours, got %q", got)
	}
	// Error AFTER our request → ours.
	after := `{"info":{"id":"msg_new","role":"assistant","time":{"completed":2},` +
		`"error":{"name":"APIError","data":{"message":"our failure","statusCode":429}}},"parts":[]}`
	got := parseOpenCodeAsyncError([]byte("["+req+","+after+"]"), "msg_ours")
	if !strings.Contains(got, "our failure") || !strings.Contains(got, "429") {
		t.Errorf("a failure after our request must be surfaced with its status, got %q", got)
	}
	// A clean completed reply must report NO error (no false positives).
	ok := `{"info":{"id":"msg_ok","role":"assistant","time":{"completed":3}},"parts":[{"type":"text","text":"[]"}]}`
	if got := parseOpenCodeAsyncError([]byte("["+req+","+ok+"]"), "msg_ours"); got != "" {
		t.Errorf("a successful generation must report no error, got %q", got)
	}
}
