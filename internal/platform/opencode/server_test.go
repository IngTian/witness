package opencode

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
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

// One job = one fresh session: create it with witness's agent and the model, prompt it with the
// corpus alone, read the reply, delete it. The system prompt travels ONLY as the agent's `system` in
// the private config (critic C1): v2's create/prompt routes have no system field, and an env var is
// capped at 32,767 chars on Windows while the lens prompts total ~36 KB.
func TestRunCreatesPromptsAndDeletesOneSessionPerJob(t *testing.T) {
	fake, srv := newFakeV2(t)
	got, err := srv.Run(context.Background(), "openai/gpt-5.5", "EXTRACT", "DATA")
	if err != nil {
		t.Fatal(err)
	}
	if got != "RESULT" {
		t.Fatalf("reply = %q, want RESULT", got)
	}
	s := fake.session("ses_1")
	wantCreate := map[string]any{"title": "witness", "agent": agentID("EXTRACT"),
		"model": map[string]any{"providerID": "openai", "id": "gpt-5.5"}}
	if !reflect.DeepEqual(s.Create, wantCreate) {
		t.Errorf("POST /api/session body = %v, want %v", s.Create, wantCreate)
	}
	if id, _ := s.Prompt["id"].(string); !strings.HasPrefix(id, "msg_") || !reflect.DeepEqual(s.Prompt, map[string]any{"id": id, "text": "DATA"}) {
		t.Errorf("prompt body = %v, want {id: msg_…, text: DATA} and nothing else", s.Prompt)
	}
	// The agent: the prompt verbatim (distill.RunnerMine already fenced it) and deny-all.
	agents, _ := fake.config()["agents"].(map[string]any)
	agent, _ := agents[agentID("EXTRACT")].(map[string]any)
	if agent["system"] != "EXTRACT" {
		t.Errorf("agents[%s].system = %v, want EXTRACT (config agents: %v)", agentID("EXTRACT"), agent["system"], agents)
	}
	wantDeny := []any{map[string]any{"action": "*", "resource": "*", "effect": "deny"}}
	if !reflect.DeepEqual(agent["permissions"], wantDeny) {
		t.Errorf("agent permissions = %v, want deny-all %v", agent["permissions"], wantDeny)
	}
	calls := fake.seen()
	deletes := 0
	for _, c := range calls {
		if strings.HasPrefix(c, "DELETE ") {
			deletes++
		}
		if strings.HasSuffix(c, "/interrupt") {
			t.Errorf("a successful job was interrupted: %v", calls)
		}
	}
	if deletes != 1 || calls[len(calls)-1] != "DELETE /api/session/ses_1" {
		t.Errorf("calls = %v, want exactly one DELETE /api/session/ses_1, last", calls)
	}
}

func TestModelRefKeepsVariantAndSlashes(t *testing.T) {
	for model, want := range map[string]map[string]string{
		"openai/gpt-5.5#high": {"providerID": "openai", "id": "gpt-5.5", "variant": "high"},
		"openrouter/a/b":      {"providerID": "openrouter", "id": "a/b"},
		" openai/gpt-5.5 ":    {"providerID": "openai", "id": "gpt-5.5"},
	} {
		got, err := splitOpenCodeModel(model)
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Errorf("splitOpenCodeModel(%q) = %v, %v; want %v", model, got, err, want)
		}
	}
	if _, err := splitOpenCodeModel(""); !errors.Is(err, ErrOpenCodeNoModel) {
		t.Errorf(`splitOpenCodeModel("") = %v, want ErrOpenCodeNoModel`, err)
	}
	for _, bad := range []string{"nomodel", "/x", "openai/", "openai/x#"} {
		if _, err := splitOpenCodeModel(bad); err == nil || errors.Is(err, ErrOpenCodeNoModel) {
			t.Errorf("splitOpenCodeModel(%q) = %v, want a provider/model format error", bad, err)
		}
	}
}

// Witness never falls back to OpenCode's default model (opencode/big-pickle may train on inputs),
// so an unnamed stage fails before any request reaches the serve, with the keys that fix it.
func TestRunRefusesAnEmptyModelBeforeAnyRequest(t *testing.T) {
	fake, srv := newFakeV2(t)
	_, err := srv.Run(context.Background(), "", "P", "D")
	if !errors.Is(err, ErrOpenCodeNoModel) {
		t.Fatalf("err = %v, want ErrOpenCodeNoModel", err)
	}
	for _, want := range []string{"witness config set mine_model", "opencode models"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("err = %v, want it to name %q", err, want)
		}
	}
	if calls := fake.seen(); len(calls) != 0 {
		t.Errorf("requests sent for an unnamed model: %v", calls)
	}
}

// A config agent shows up in a running serve ~0.25 s after the write, so the session is created
// only once /api/agent lists it; before that a prompt fails with "Agent not found".
func TestRunWaitsForANewAgentToLoad(t *testing.T) {
	fake, srv := newFakeV2(t)
	fake.agentDelay = 3
	if _, err := srv.Run(context.Background(), "openai/gpt-5.5", "P", "D"); err != nil {
		t.Fatal(err)
	}
	calls := fake.seen()
	create := slices.Index(calls, "POST /api/session")
	if create < 0 || strings.Count(strings.Join(calls[:create], "\n"), "GET /api/agent") != fake.agentDelay+1 {
		t.Errorf("calls = %v; want POST /api/session right after the first /api/agent poll that lists the agent (poll %d)", calls, fake.agentDelay+1)
	}
}

func TestRunReusesTheAgentForTheSamePrompt(t *testing.T) {
	fake, srv := newFakeV2(t)
	agentPolls := func() int { return strings.Count(strings.Join(fake.seen(), "\n"), "GET /api/agent") }
	if _, err := srv.Run(context.Background(), "openai/gpt-5.5", "P", "D1"); err != nil {
		t.Fatal(err)
	}
	after1 := agentPolls()
	if _, err := srv.Run(context.Background(), "openai/gpt-5.5", "P", "D2"); err != nil {
		t.Fatal(err)
	}
	if got := agentPolls(); got != after1 {
		t.Errorf("/api/agent polls %d after the second Run, %d after the first: a known agent was re-loaded", got, after1)
	}
	if agents, _ := fake.config()["agents"].(map[string]any); len(agents) != 1 {
		t.Errorf("config agents = %v, want exactly one for one distinct prompt", agents)
	}
}

func TestRunFailsWhenTheAgentNeverLoads(t *testing.T) {
	prev := healthCheckBudget
	healthCheckBudget = 200 * time.Millisecond
	t.Cleanup(func() { healthCheckBudget = prev })
	fake, srv := newFakeV2(t)
	fake.agentDelay = math.MaxInt
	_, err := srv.Run(context.Background(), "openai/gpt-5.5", "P", "D")
	if err == nil || !strings.Contains(err.Error(), "did not load") {
		t.Fatalf("err = %v, want the agent named as not loaded", err)
	}
	if slices.Contains(fake.seen(), "POST /api/session") {
		t.Errorf("a session was created for an agent the serve never listed: %v", fake.seen())
	}
}

// Every failure ends the turn upstream (interrupt) before the session goes (delete).
func TestRunInterruptsThenDeletesOnAProviderError(t *testing.T) {
	fake, srv := newFakeV2(t)
	fail := map[string]any{"type": "provider.internal", "message": "mock internal error", "status": 500}
	fake.turn = func(s *fakeSession) []map[string]any {
		a := assistantRec("", nil)
		a["error"] = fail
		return []map[string]any{s.user(), a, idleRec("failed", fail)}
	}
	_, err := srv.Run(context.Background(), "openai/gpt-5.5", "P", "D")
	if err == nil || !strings.Contains(err.Error(), "provider.internal") {
		t.Fatalf("err = %v, want the provider error named", err)
	}
	assertInterruptedThenDeleted(t, fake.seen(), "ses_1")
}

// assertInterruptedThenDeleted checks a failed job's last two requests.
func assertInterruptedThenDeleted(t *testing.T, calls []string, id string) {
	t.Helper()
	want := []string{"POST /api/session/" + id + "/interrupt", "DELETE /api/session/" + id}
	if tail := calls[max(len(calls)-3, 0):]; len(calls) < 2 || !slices.Equal(calls[len(calls)-2:], want) {
		t.Errorf("calls end with %v (of %d), want %v", tail, len(calls), want)
	}
}

// replyForMessage is the native resume's harvest: a reply counts only once its turn is idle
// `succeeded`. A dead turn (a SIGKILLed serve leaves [user, assistant] with no idle) or a failed
// one yields "", so the caller re-prompts instead of committing a partial or empty reply.
func TestReplyForMessageHarvestsOnlyASucceededTurn(t *testing.T) {
	fake, srv := newFakeV2(t)
	agent, err := srv.ensureAgent(context.Background(), "P")
	if err != nil {
		t.Fatal(err)
	}
	id, err := srv.createSession(context.Background(), "openai/gpt-5.5", agent)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := srv.doJSON(context.Background(), http.MethodPost, "/api/session/"+id+"/prompt",
		map[string]string{"id": "msg_r", "text": "D"}, http.StatusOK); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		turn func(*fakeSession) []map[string]any
		want string
	}{
		{"succeeded", okTurn("[1]"), "[1]"},
		{"dead", func(s *fakeSession) []map[string]any { return []map[string]any{s.user(), assistantRec("partial", nil)} }, ""},
		{"failed", func(s *fakeSession) []map[string]any {
			return []map[string]any{s.user(), assistantRec("partial", nil), idleRec("failed", nil)}
		}, ""},
	} {
		fake.mu.Lock()
		fake.turn = tc.turn
		fake.mu.Unlock()
		if got, err := srv.replyForMessage(context.Background(), id, "msg_r"); err != nil || got != tc.want {
			t.Errorf("%s: replyForMessage = %q, %v; want %q", tc.name, got, err, tc.want)
		}
	}
}

// privateConfigOverlay is the contract job's seam (PR 3 adds a mock provider through it): merged
// beside witness's agents, and absent by default so the private serve gets no provider config.
func TestPrivateConfigOverlayIsMerged(t *testing.T) {
	path := filepath.Join(t.TempDir(), "opencode.json")
	read := func() map[string]any {
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var cfg map[string]any
		if err := json.Unmarshal(b, &cfg); err != nil {
			t.Fatal(err)
		}
		return cfg
	}
	if err := writePrivateConfig(path, map[string]string{"w-a": "A"}); err != nil {
		t.Fatal(err)
	}
	if cfg := read(); len(cfg) != 2 || cfg["$schema"] != "https://opencode.ai/config.json" {
		t.Errorf("config = %v, want only $schema and agents with no overlay", cfg)
	}
	privateConfigOverlay = map[string]any{"provider": map[string]any{"mock": map[string]any{}}}
	t.Cleanup(func() { privateConfigOverlay = nil })
	if err := writePrivateConfig(path, map[string]string{"w-a": "A"}); err != nil {
		t.Fatal(err)
	}
	cfg := read()
	if p, _ := cfg["provider"].(map[string]any); p["mock"] == nil {
		t.Errorf("config = %v, want provider.mock from the overlay", cfg)
	}
	if a, _ := cfg["agents"].(map[string]any); a["w-a"] == nil {
		t.Errorf("config = %v, want the agents beside the overlay", cfg)
	}
}
