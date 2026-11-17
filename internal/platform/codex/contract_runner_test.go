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
	var mu sync.Mutex
	var requests []map[string]json.RawMessage
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Method == "GET" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(catalog)
			return
		}
		if req.Method != "POST" || req.URL.Path != "/v1/responses" || req.Header.Get("Authorization") != "Bearer dummy" {
			http.Error(w, "unexpected provider request", http.StatusBadRequest)
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
	defer func() { commandContext = previous }()
	for _, tc := range []struct {
		slug        string
		dropCatalog bool
	}{
		{"gpt-6.1-sol", false}, {"gpt-5.5", false}, {"gpt-6.1-sol", true},
	} {
		name := tc.slug
		if tc.dropCatalog {
			name += "/without-catalog-override"
		}
		t.Run(name, func(t *testing.T) {
			mu.Lock()
			requests = nil
			mu.Unlock()
			commandContext = func(ctx context.Context, binary string, args ...string) *exec.Cmd {
				forwarded := []string{"-c", configPath("openai_base_url", srv.URL+"/v1")}
				for i := 0; i < len(args); i++ {
					if tc.dropCatalog && args[i] == "-c" && i+1 < len(args) && strings.HasPrefix(args[i+1], "model_catalog_json=") {
						i++
						continue
					}
					forwarded = append(forwarded, args[i])
				}
				return exec.CommandContext(ctx, binary, forwarded...)
			}
			r := (Platform{}).NewRunner(store.Config{RuntimeRoot: t.TempDir(), RunnerModels: []string{tc.slug}})
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			if err := r.Open(ctx); err != nil {
				t.Fatal(err)
			}
			defer r.Close()
			answer, err := r.Run(ctx, tc.slug, "WITNESS-SYSTEM-ONLY", "WITNESS-CORPUS-ONLY")
			if err != nil || answer != "WITNESS-PROBE-OK" {
				t.Fatalf("answer %q, error %v", answer, err)
			}
			mu.Lock()
			defer mu.Unlock()
			if len(requests) == 0 {
				t.Fatal("no generation request captured")
			}
			caught := false
			for _, req := range requests {
				if string(req["model"]) != fmt.Sprintf("%q", tc.slug) {
					t.Fatalf("unexpected model %s", req["model"])
				}
				err := checkWireRequest(req)
				if tc.dropCatalog {
					if err != nil && strings.Contains(err.Error(), "additional_tools") {
						caught = true
					}
				} else if err != nil {
					t.Fatal(err)
				}
			}
			if tc.dropCatalog && !caught {
				t.Fatal("dropping model_catalog_json did not trigger the additional_tools assertion")
			}
		})
	}
}

func checkWireRequest(req map[string]json.RawMessage) error {
	var offered []json.RawMessage
	if raw := req["tools"]; len(raw) > 0 {
		if err := json.Unmarshal(raw, &offered); err != nil {
			return err
		}
	}
	if len(offered) != 0 {
		return fmt.Errorf("tools offered: %s", req["tools"])
	}
	var input []struct {
		Type, Role string
		Content    []struct{ Type, Text string }
	}
	if err := json.Unmarshal(req["input"], &input); err != nil {
		return err
	}
	dev, user := 0, 0
	for _, msg := range input {
		if msg.Type != "message" {
			return fmt.Errorf("unexpected wire input item: %s", msg.Type)
		}
		if len(msg.Content) != 1 || msg.Content[0].Type != "input_text" {
			return fmt.Errorf("unexpected %s message content", msg.Role)
		}
		switch msg.Role {
		case "developer":
			if msg.Content[0].Text != "WITNESS-SYSTEM-ONLY" {
				return fmt.Errorf("unexpected developer instructions")
			}
			dev++
		case "user":
			if msg.Content[0].Text != "WITNESS-CORPUS-ONLY" {
				return fmt.Errorf("unexpected corpus")
			}
			user++
		default:
			return fmt.Errorf("unexpected message role %q", msg.Role)
		}
	}
	if dev == 0 || user != 1 {
		return fmt.Errorf("instruction/corpus channels: developer=%d user=%d", dev, user)
	}
	return nil
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
