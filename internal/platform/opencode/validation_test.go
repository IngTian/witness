package opencode

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/IngTian/witness/internal/platform"
	"github.com/IngTian/witness/internal/store"
)

// OpenCode 2.4 answers ready before its model catalog loads, and GET /api/model lists nothing for
// about a second; one read refused every model (the 2026-11-23 contract run on 2.4.1).
func TestValidateModelsWaitsForTheCatalogToLoad(t *testing.T) {
	fake, srv := newFakeV2(t)
	fake.models = []map[string]any{{"providerID": "mock", "id": "ok", "variants": []map[string]any{{"id": "high"}}}}
	fake.emptyModelPolls = 3
	if err := srv.validateModels(context.Background(), []string{"mock/ok", "mock/ok#high"}); err != nil {
		t.Fatalf("validateModels = %v, want nil once the catalog loads", err)
	}
}

func TestValidateModelsAgainstTheServesList(t *testing.T) {
	prev := healthCheckBudget
	healthCheckBudget = 300 * time.Millisecond // a refusal waits the whole budget for the model to load
	t.Cleanup(func() { healthCheckBudget = prev })
	fake, srv := newFakeV2(t)
	fake.models = []map[string]any{
		{"providerID": "openai", "id": "gpt-5.5", "variants": []map[string]any{{"id": "high"}}},
		{"providerID": "opencode", "id": "glm-5.3-flash-free"},
		{"providerID": "anthropic", "id": "claude-sonnet-5-5"},
	}
	fake.envNames = map[string][]string{
		"anthropic": {"ANTHROPIC_API_KEY"},
	}

	// Available models: should pass
	ctx := context.Background()
	if err := srv.validateModels(ctx, []string{"openai/gpt-5.5", "openai/gpt-5.5#high", "opencode/glm-5.3-flash-free"}); err != nil {
		t.Errorf("validateModels on available models = %v, want nil", err)
	}

	// Unavailable anthropic model: should mention env var
	err := srv.validateModels(ctx, []string{"anthropic/claude-x"})
	if err == nil {
		t.Fatal("validateModels on unavailable anthropic model = nil, want error")
	}
	msg := err.Error()
	for _, want := range []string{"anthropic/claude-x", "ANTHROPIC_API_KEY", "opencode auth login", "AWS_PROFILE", "available: anthropic/claude-sonnet-5-5"} {
		if !strings.Contains(msg, want) {
			t.Errorf("unavailable anthropic model error missing %q: %s", want, msg)
		}
	}

	// Unavailable opencode free model: should name `opencode models`, NOT "API key"
	err = srv.validateModels(ctx, []string{"opencode/crouton-free"})
	if err == nil {
		t.Fatal("validateModels on unavailable opencode model = nil, want error")
	}
	msg = err.Error()
	if !strings.Contains(msg, "opencode models") {
		t.Errorf("unavailable opencode model error missing 'opencode models': %s", msg)
	}
	if strings.Contains(msg, "API key") {
		t.Errorf("unavailable opencode model error should NOT mention 'API key': %s", msg)
	}
}

func TestUnavailableHintSurvivesAMissingIntegrationRoute(t *testing.T) {
	prev := healthCheckBudget
	healthCheckBudget = 300 * time.Millisecond // a refusal waits the whole budget for the model to load
	t.Cleanup(func() { healthCheckBudget = prev })
	fake, srv := newFakeV2(t)
	fake.models = []map[string]any{
		{"providerID": "unknown", "id": "some-model"},
	}
	// No envNames entry for "other", so GET /api/integration/other is a 404

	ctx := context.Background()
	err := srv.validateModels(ctx, []string{"other/missing"})
	if err == nil {
		t.Fatal("validateModels on model with missing integration = nil, want error")
	}
	msg := err.Error()
	// Should still name the three credential kinds
	for _, want := range []string{"opencode auth login", "AWS_PROFILE"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error missing %q: %s", want, msg)
		}
	}
	if strings.Contains(msg, "(for ") {
		t.Errorf("error should NOT include a provider-specific env var hint when the route fails: %s", msg)
	}
	if !slices.Contains(fake.seen(), "GET /api/integration/other") {
		t.Errorf("the integration route was never asked: %v", fake.seen())
	}
}

// StartOpenCodeServerIn refuses an unnamed stage, even after a named one, and a model without its
// provider/ before it spawns anything.
func TestStartRefusesABadModelBeforeSpawning(t *testing.T) {
	prevExec, prevVersion := execCommandContext, openCodeVersionCommand
	t.Cleanup(func() { execCommandContext, openCodeVersionCommand = prevExec, prevVersion })
	openCodeVersionCommand = func(context.Context, string) ([]byte, error) {
		return []byte("opencode v2.3.0"), nil
	}
	execCommandContext = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		t.Errorf("spawned %s %v for a model it must refuse", name, args)
		return exec.CommandContext(ctx, "/nonexistent")
	}
	for _, tc := range []struct {
		models  []string
		refused func(error) bool
	}{
		{[]string{"openai/x", ""}, func(err error) bool { return errors.Is(err, ErrOpenCodeNoModel) }},
		{[]string{"gpt-5.5"}, func(err error) bool { return err != nil && strings.Contains(err.Error(), "provider/model") }},
	} {
		if _, err := StartOpenCodeServerIn(context.Background(), t.TempDir(), tc.models); !tc.refused(err) {
			t.Errorf("StartOpenCodeServerIn(%q) = %v, want it refused", tc.models, err)
		}
	}
}

func TestRunnerValidateModelsBootsAndClosesItsServe(t *testing.T) {
	t.Setenv(platform.DisableExternalRunnersEnv, "")
	useFakeServe(t, "lease")

	root := t.TempDir()
	r := &runner{cfg: store.Config{RuntimeRoot: root}}

	// Each call boots its own serve and closes it again, on success and on failure alike.
	for _, tc := range []struct {
		model   string
		wantErr bool
	}{{"openai/gpt-5.5", false}, {"opencode/crouton-free", true}} {
		if err := r.ValidateModels(context.Background(), tc.model); (err != nil) != tc.wantErr {
			t.Errorf("ValidateModels(%s) = %v, want an error: %v", tc.model, err, tc.wantErr)
		}
		if entries, err := os.ReadDir(filepath.Join(root, "opencode-serve")); err != nil && !os.IsNotExist(err) || len(entries) != 0 {
			t.Errorf("serve dir after ValidateModels(%s) = %d entries (%v), want 0", tc.model, len(entries), err)
		}
	}
}

// seedFiles creates each path under root (parents included) with a little content.
func seedFiles(t *testing.T, root string, paths ...string) {
	t.Helper()
	for _, p := range paths {
		p = filepath.Join(root, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// An empty root must remove nothing: relative to "", the v1 paths are the CWD's, and the worker's
// cwd is a user repo. Open refuses an empty root first today; this keeps the deleter safe alone.
func TestRemoveRetiredRuntimeNeedsARoot(t *testing.T) {
	cwd := t.TempDir()
	t.Chdir(cwd)
	seedFiles(t, cwd, "xdg/opencode/auth.json", "opencode.db")
	removeRetiredRuntime("")
	for _, p := range []string{"xdg/opencode/auth.json", "opencode.db"} {
		if _, err := os.Stat(filepath.Join(cwd, filepath.FromSlash(p))); err != nil {
			t.Errorf("removeRetiredRuntime(\"\") removed the cwd's %s: %v", p, err)
		}
	}
}
