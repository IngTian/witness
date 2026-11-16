//go:build opencodecontract

package opencode

// The mock provider: an OpenAI-compatible endpoint on loopback whose MODEL id picks the behaviour,
// so a contract test reaches every reply and error shape without a model call. The chunk sequence
// is the one the evidence round's mock proved against real 2.2.7 and 2.3.0; the error bodies are
// the gateway's and OpenAI's own (freetier.md §5).

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// mockCall is one chat request as the provider saw it.
type mockCall struct {
	Model, System, User string
	Tools               int
	ToolNames           []string
	RawTools            string // raw JSON of tools array, for debugging non-zero tool offers
	Stream              bool
}

type mockProvider struct {
	URL string

	mu       sync.Mutex
	calls    []mockCall
	inFlight int
	peak     int
}

// mockErrors are the 429s, by model. flaky's body must never say "exhausted": on 2.2.7 a body
// containing "rate limit exhausted" is a terminal provider.quota after one request, so the retry
// under test would never happen.
var mockErrors = map[string]struct{ retryAfter, body string }{
	"free":      {"3600", `{"type":"error","error":{"type":"FreeUsageLimitError","message":"Rate limit exceeded. Please try again later."}}`},
	"exhausted": {"2", `{"error":{"message":"User 10s request rate limit exhausted (3 of 3).","type":"rate_limit_exceeded"}}`},
	"flaky":     {"1", `{"error":{"message":"Rate limit reached for requests","type":"rate_limit_exceeded"}}`},
}

// startMock serves, by model: ok streams "MOCK-REPLY ok"; slow waits ~2 s, then streams "MOCK-REPLY
// slow"; free, exhausted answer their 429; flaky answers 429 on odd calls and like ok on even ones.
// A non-stream request (a title) gets "MOCK-TITLE".
func startMock(t *testing.T) *mockProvider {
	t.Helper()
	m := &mockProvider{}
	srv := httptest.NewServer(http.HandlerFunc(m.serveHTTP)) // 127.0.0.1:0
	t.Cleanup(srv.Close)
	m.URL = srv.URL
	return m
}

func (m *mockProvider) serveHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet && r.URL.Path == "/v1/models" {
		var data []map[string]string
		for _, id := range []string{"ok", "slow", "free", "exhausted", "flaky"} {
			data = append(data, map[string]string{"id": id, "object": "model"})
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": data})
		return
	}
	if r.Method != http.MethodPost || r.URL.Path != "/v1/chat/completions" {
		http.NotFound(w, r)
		return
	}
	var req struct {
		Model    string            `json:"model"`
		Stream   bool              `json:"stream"`
		Tools    []json.RawMessage `json:"tools"`
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	call := mockCall{Model: req.Model, Tools: len(req.Tools), Stream: req.Stream}
	if len(req.Tools) > 0 {
		if b, err := json.Marshal(req.Tools); err == nil {
			call.RawTools = string(b)
		}
	}
	for _, raw := range req.Tools {
		var tl struct {
			Name     string `json:"name"`
			Function struct {
				Name string `json:"name"`
			} `json:"function"`
		}
		_ = json.Unmarshal(raw, &tl)
		call.ToolNames = append(call.ToolNames, cmp.Or(tl.Function.Name, tl.Name))
	}
	var system, user []string
	for _, msg := range req.Messages {
		switch msg.Role {
		case "system", "developer":
			system = append(system, contentText(msg.Content))
		case "user":
			user = append(user, contentText(msg.Content))
		}
	}
	call.System, call.User = strings.Join(system, "\n"), strings.Join(user, "\n")

	m.mu.Lock()
	m.calls = append(m.calls, call)
	n := 0
	for _, c := range m.calls {
		if c.Model == req.Model {
			n++
		}
	}
	m.inFlight++
	m.peak = max(m.peak, m.inFlight)
	m.mu.Unlock()
	defer func() {
		m.mu.Lock()
		m.inFlight--
		m.mu.Unlock()
	}()

	if e, ok := mockErrors[req.Model]; ok && (req.Model != "flaky" || n%2 == 1) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Retry-After", e.retryAfter)
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(e.body))
		return
	}
	usage := map[string]int{"prompt_tokens": 10, "completion_tokens": 3, "total_tokens": 13}
	if !req.Stream {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "c", "object": "chat.completion", "created": 0, "model": req.Model,
			"choices": []any{map[string]any{"index": 0, "message": map[string]string{"role": "assistant", "content": "MOCK-TITLE"}, "finish_reason": "stop"}},
			"usage":   usage})
		return
	}
	if req.Model == "slow" {
		select {
		case <-time.After(2 * time.Second):
		case <-r.Context().Done():
			return
		}
	}
	w.Header().Set("Content-Type", "text/event-stream")
	flusher, _ := w.(http.Flusher)
	chunk := func(delta map[string]string, finish any, usage any) {
		o := map[string]any{"id": "c", "object": "chat.completion.chunk", "created": time.Now().Unix(), "model": req.Model,
			"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}}}
		if usage != nil {
			o["usage"] = usage
		}
		b, _ := json.Marshal(o)
		fmt.Fprintf(w, "data: %s\n\n", b)
		flusher.Flush()
	}
	chunk(map[string]string{"role": "assistant", "content": "MOCK-REPLY "}, nil, nil)
	chunk(map[string]string{"content": req.Model}, nil, nil)
	chunk(map[string]string{}, "stop", usage)
	fmt.Fprint(w, "data: [DONE]\n\n")
	flusher.Flush()
}

// contentText reads a chat message's content: a string, or an array of {type, text} parts.
func contentText(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var parts []struct {
		Text string `json:"text"`
	}
	_ = json.Unmarshal(raw, &parts)
	var b strings.Builder
	for _, p := range parts {
		b.WriteString(p.Text)
	}
	return b.String()
}

// Calls returns the chat requests made for model, oldest first.
func (m *mockProvider) Calls(model string) []mockCall {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []mockCall
	for _, c := range m.calls {
		if c.Model == model {
			out = append(out, c)
		}
	}
	return out
}

// PeakInFlight is the most chat requests the mock was serving at once.
func (m *mockProvider) PeakInFlight() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.peak
}

// ProviderConfig is the mock as an opencode.json provider (the legacy shape v2 migrates). ok
// declares a #high variant, so the runner's variant ref is testable.
func (m *mockProvider) ProviderConfig() string {
	return `{"provider":{"mock":{"npm":"@ai-sdk/openai-compatible","name":"Mock","options":{"baseURL":"` + m.URL + `/v1","apiKey":"mock-key"},` +
		`"models":{"ok":{"variants":{"high":{}}},"slow":{},"free":{},"exhausted":{},"flaky":{}}}}}`
}

// installSystemProvider writes the mock as OpenCode's machine-wide opencode.json, the one config
// witness's private serve still loads, so a worker-started serve (another process) sees the mock.
// It never overwrites an existing file.
func installSystemProvider(t *testing.T, m *mockProvider) {
	t.Helper()
	var path string
	switch runtime.GOOS {
	case "windows":
		t.Setenv("ProgramData", t.TempDir()) // procEnv re-reads it for every spawn
		path = filepath.Join(os.Getenv("ProgramData"), "opencode", "opencode.json")
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
	case "linux":
		path = "/etc/opencode/opencode.json"
	default:
		t.Skipf("the machine-wide OpenCode config on %s (/Library/Application Support/opencode) needs root", runtime.GOOS)
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if errors.Is(err, fs.ErrExist) {
		t.Fatalf("%s already exists: most likely a stale mock from a contract run killed before its cleanup removed it. Check and delete it by hand; this test never overwrites it (it could be a real machine-wide config)", path)
	}
	if err != nil {
		t.Fatalf("create %s: %v (the contract workflow's `sudo install -d -o \"$(id -u)\" /etc/opencode` step makes it writable)", path, err)
	}
	t.Cleanup(func() { _ = os.Remove(path) })
	_, err = f.WriteString(m.ProviderConfig())
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		t.Fatal(err)
	}
}
