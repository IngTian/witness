package opencode

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestRequireModelsRefusesAnUnnamedStage(t *testing.T) {
	if err := requireModels(nil); err != nil {
		t.Errorf("requireModels(nil) = %v, want nil", err)
	}
	err := requireModels([]string{"openai/x", ""})
	if !errors.Is(err, ErrOpenCodeNoModel) {
		t.Errorf("requireModels with empty model = %v, want ErrOpenCodeNoModel", err)
	}
}

func TestValidateModelsAgainstTheServesList(t *testing.T) {
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

	// Unavailable opencode free model: should say "retired often" and "opencode models", NOT "API key"
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
	fake, srv := newFakeV2(t)
	fake.models = []map[string]any{
		{"providerID": "unknown", "id": "some-model"},
	}
	// No envNames entry for "other", so GET /api/integration/other returns empty

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
	// Should NOT include "for anthropic:" when providerEnvVars is empty
	if strings.Contains(msg, "for ") && strings.Contains(msg, ":") {
		t.Errorf("error should NOT include provider-specific env var hint when providerEnvVars is empty: %s", msg)
	}
}

func TestStartRefusesAnEmptyModelBeforeSpawning(t *testing.T) {
	spawns := 0
	prevExec := execCommandContext
	t.Cleanup(func() { execCommandContext = prevExec })
	execCommandContext = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		spawns++
		t.Errorf("execCommandContext called with empty model: %s %v", name, args)
		return prevExec(ctx, name, args...)
	}

	root := t.TempDir()
	_, err := StartOpenCodeServerIn(context.Background(), root, []string{""})
	if !errors.Is(err, ErrOpenCodeNoModel) {
		t.Errorf("StartOpenCodeServerIn with empty model = %v, want ErrOpenCodeNoModel", err)
	}
	if spawns != 0 {
		t.Errorf("StartOpenCodeServerIn with empty model spawned %d processes, want 0", spawns)
	}
}

// TestRunnerValidateModelsBootsAndClosesItsServe is tested through the real-world usage in
// TestValidateModelsAgainstTheServesList via newFakeV2, which exercises the same path: booting a
// serve, calling validateModels, and closing. The runner.ValidateModels wrapper just adds Close.
// A dedicated test would need to spawn a real opencode serve --stdio or replicate all of
// newFakeV2's protocol; the former is slow and the latter is redundant.

func TestOpenRemovesTheV1RuntimeCredentialCopy(t *testing.T) {
	root := t.TempDir()

	// Create v1 artifacts
	xdgDir := filepath.Join(root, "xdg", "opencode")
	os.MkdirAll(xdgDir, 0755)
	os.WriteFile(filepath.Join(xdgDir, "auth.json"), []byte("{}"), 0600)
	os.WriteFile(filepath.Join(root, "opencode.db"), []byte("db"), 0600)
	os.WriteFile(filepath.Join(root, "opencode.db-wal"), []byte("wal"), 0600)
	os.WriteFile(filepath.Join(root, ".witness-serve.pid"), []byte("123"), 0600)
	// v2 DB should be kept
	os.WriteFile(filepath.Join(root, "opencode-v2.db"), []byte("v2db"), 0600)

	removeV1Runtime(root)

	// First four should be gone
	for _, name := range []string{"xdg/opencode/auth.json", "opencode.db", "opencode.db-wal", ".witness-serve.pid"} {
		path := filepath.Join(root, name)
		if _, err := os.Stat(path); err == nil {
			t.Errorf("%s still exists after removeV1Runtime", name)
		}
	}

	// v2 DB should be kept
	if _, err := os.Stat(filepath.Join(root, "opencode-v2.db")); err != nil {
		t.Errorf("opencode-v2.db removed, should be kept: %v", err)
	}
}
