package distill

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestBuildOpenCodeServeCmdIsolation(t *testing.T) {
	cmd := buildOpenCodeServeCmd(context.Background(), 12345, "secret")
	for _, want := range []string{"serve", "--pure", "--hostname", "127.0.0.1", "--port", "12345"} {
		if !slices.Contains(cmd.Args, want) {
			t.Fatalf("missing %q in args: %v", want, cmd.Args)
		}
	}
	joinedEnv := strings.Join(cmd.Env, "\n")
	for _, want := range []string{"WITNESS_WORKER=1", "OPENCODE_DISABLE_CLAUDE_CODE=1", "OPENCODE_SERVER_PASSWORD=secret", "OPENCODE_CONFIG_CONTENT="} {
		if !strings.Contains(joinedEnv, want) {
			t.Fatalf("missing %q in env: %s", want, joinedEnv)
		}
	}
	if !strings.Contains(joinedEnv, `"permission":{"*":"deny"}`) {
		t.Fatalf("agent permission config should deny tools: %s", joinedEnv)
	}
}

func TestOpenCodeServerRunCreatesSendsAndDeletesSession(t *testing.T) {
	var created, prompted, deleted bool
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
			if body["title"] != "witness-distill" || body["agent"] != openCodeAgentName {
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
			if body["agent"] != openCodeAgentName {
				t.Fatalf("bad agent: %#v", body["agent"])
			}
			if system := body["system"].(string); !strings.Contains(system, "EXTRACT") || !strings.Contains(system, "UNTRUSTED") {
				t.Fatalf("system prompt missing trusted/untrusted split: %q", system)
			}
			parts := body["parts"].([]any)
			part := parts[0].(map[string]any)
			if part["type"] != "text" || !strings.Contains(part["text"].(string), "<witness:untrusted>") || !strings.Contains(part["text"].(string), "DATA") {
				t.Fatalf("bad parts: %#v", body["parts"])
			}
			model := body["model"].(map[string]any)
			if model["providerID"] != "openai" || model["modelID"] != "gpt-5.5" {
				t.Fatalf("bad message model: %#v", model)
			}
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodGet && r.URL.Path == "/session/ses_test/message":
			_, _ = w.Write([]byte(`[
				{"info":{"id":"msg_request","role":"user"},"parts":[{"id":"prt_u","type":"text","text":"DATA"}]},
				{"info":{"id":"msg_reply","role":"assistant"},"parts":[{"id":"prt_1","type":"text","text":"RESULT"}]}
			]`))
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
	oldPoll := openCodeAsyncPollInterval
	openCodeAsyncPollInterval = time.Millisecond
	defer func() { openCodeAsyncPollInterval = oldPoll }()

	var aborted, deleted bool
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/session":
			_, _ = w.Write([]byte(`{"id":"ses_timeout"}`))
		case r.Method == http.MethodPost && r.URL.Path == "/session/ses_timeout/prompt_async":
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodGet && r.URL.Path == "/session/ses_timeout/message":
			_, _ = w.Write([]byte(`[]`))
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

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Millisecond)
	defer cancel()
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
		{"info":{"id":"msg_request","role":"user"},"parts":[{"id":"p1","type":"text","text":"input"}]},
		{"info":{"id":"msg_reply","role":"assistant"},"parts":[{"id":"p2","type":"text","text":"output"}]}
	]`)
	if got := parseOpenCodeAsyncReply(data, "msg_request"); got != "output" {
		t.Fatalf("got %q", got)
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

func TestRunWithRejectsUnknownRunner(t *testing.T) {
	if _, err := RunWith(context.Background(), "bogus", "", "", ""); err == nil {
		t.Fatalf("unknown runner should fail")
	}
}
