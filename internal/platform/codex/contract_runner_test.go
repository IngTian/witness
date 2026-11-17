//go:build codexcontract

package codex

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/IngTian/witness/internal/platform"
	"github.com/IngTian/witness/internal/store"
)

// Real CLI, fake endpoint: inspect the request Run actually sends, including
// model-catalog-driven tools and developer schema messages. No account is used.
func TestContractToolFreeWire(t *testing.T) {
	t.Setenv(platform.DisableExternalRunnersEnv, "0")
	t.Setenv("CODEX_API_KEY", "dummy")
	t.Setenv("CODEX_HOME", t.TempDir())
	bin, err := FindBinary()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(context.Background(), bin, "debug", "models", "--bundled")
	cmd.Env = childEnv(t.TempDir())
	catalog, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	var models struct{ Models []struct{ Slug string } }
	if err := json.Unmarshal(catalog, &models); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var requests []map[string]json.RawMessage
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Method == "GET" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(catalog)
			return
		}
		var body map[string]json.RawMessage
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		mu.Lock()
		requests = append(requests, body)
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		item := map[string]any{"id": "msg_test", "type": "message", "status": "completed", "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": "WITNESS-PROBE-OK", "annotations": []any{}}}}
		for _, e := range []map[string]any{
			{"type": "response.created", "response": map[string]any{"id": "resp_test", "status": "in_progress", "output": []any{}}},
			{"type": "response.output_item.done", "output_index": 0, "item": item},
			{"type": "response.completed", "response": map[string]any{"id": "resp_test", "status": "completed", "output": []any{item}, "usage": map[string]int{"input_tokens": 10, "output_tokens": 5, "total_tokens": 15}}},
		} {
			b, _ := json.Marshal(e)
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", e["type"], b)
		}
	}))
	defer srv.Close()
	previous := commandContext
	commandContext = func(ctx context.Context, binary string, args ...string) *exec.Cmd {
		return exec.CommandContext(ctx, binary, append([]string{"-c", configPath("openai_base_url", srv.URL+"/v1")}, args...)...)
	}
	defer func() { commandContext = previous }()
	for _, m := range models.Models {
		if m.Slug != "gpt-6.1-sol" && m.Slug != "gpt-5.5" {
			continue
		}
		t.Run(m.Slug, func(t *testing.T) {
			r := (Platform{}).NewRunner(store.Config{RuntimeRoot: t.TempDir(), RunnerModels: []string{m.Slug}})
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			if err := r.Open(ctx); err != nil {
				t.Fatal(err)
			}
			defer r.Close()
			answer, err := r.Run(ctx, m.Slug, "WITNESS-SYSTEM-ONLY", "WITNESS-CORPUS-ONLY")
			if err != nil || answer != "WITNESS-PROBE-OK" {
				t.Fatalf("answer %q, error %v", answer, err)
			}
		})
	}
	mu.Lock()
	defer mu.Unlock()
	if len(requests) == 0 {
		t.Fatal("no requests captured")
	}
	for _, req := range requests {
		var offered []any
		if raw := req["tools"]; len(raw) > 0 {
			if err := json.Unmarshal(raw, &offered); err != nil {
				t.Fatal(err)
			}
		}
		if len(offered) != 0 {
			t.Fatalf("tools offered: %s", req["tools"])
		}
		var input []struct {
			Role    string
			Content []struct{ Text string }
		}
		if err := json.Unmarshal(req["input"], &input); err != nil {
			t.Fatal(err)
		}
		dev, user := 0, 0
		for _, msg := range input {
			if msg.Role != "developer" && msg.Role != "user" {
				continue
			}
			for _, part := range msg.Content {
				if msg.Role == "developer" {
					dev++
					if strings.TrimSpace(part.Text) != "WITNESS-SYSTEM-ONLY" {
						t.Fatalf("unexpected developer instructions/tool schema: %s", part.Text)
					}
				}
				if msg.Role == "user" {
					user++
					if part.Text != "WITNESS-CORPUS-ONLY" {
						t.Fatalf("unexpected corpus: %s", part.Text)
					}
				}
			}
		}
		if dev == 0 || user != 1 {
			t.Fatalf("instruction/corpus channels: developer=%d user=%d", dev, user)
		}
	}
}

// Opt-in only, uses an already-authorized witness-owned login. Never run by CI.
func TestContractLiveLogin(t *testing.T) {
	root := os.Getenv("WITNESS_CODEX_LIVE_ROOT")
	if root == "" {
		t.Skip("set WITNESS_CODEX_LIVE_ROOT to a witness-owned archive")
	}
	t.Setenv(platform.DisableExternalRunnersEnv, "0")
	t.Setenv("CODEX_API_KEY", "")
	model := os.Getenv("WITNESS_CODEX_LIVE_MODEL")
	if model == "" {
		t.Fatal("set WITNESS_CODEX_LIVE_MODEL to an available model slug")
	}
	r := (Platform{}).NewRunner(store.Config{RuntimeRoot: root, RunnerModels: []string{model}})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	if err := r.Open(ctx); err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	answer, err := r.Run(ctx, model, "Whatever the user writes, reply with exactly WITNESS-PROBE-OK and nothing else.", "What is 2+2?")
	if err != nil || answer != "WITNESS-PROBE-OK" {
		t.Fatalf("answer %q, error %v", answer, err)
	}
}
