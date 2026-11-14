package opencode

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/IngTian/witness/internal/platform"
	"github.com/IngTian/witness/internal/store"
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
	prevExec, prevVersion := execCommandContext, openCodeVersionCommand
	t.Cleanup(func() { execCommandContext, openCodeVersionCommand = prevExec, prevVersion })
	openCodeVersionCommand = func(context.Context, string) ([]byte, error) {
		return []byte("opencode v2.3.0"), nil
	}
	execCommandContext = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		t.Errorf("execCommandContext called with empty model: %s %v", name, args)
		return exec.CommandContext(ctx, "/nonexistent")
	}

	root := t.TempDir()
	_, err := StartOpenCodeServerIn(context.Background(), root, []string{""})
	if !errors.Is(err, ErrOpenCodeNoModel) {
		t.Errorf("StartOpenCodeServerIn with empty model = %v, want ErrOpenCodeNoModel", err)
	}
}

func TestStartRefusesBadFormatBeforeSpawning(t *testing.T) {
	prevExec, prevVersion := execCommandContext, openCodeVersionCommand
	t.Cleanup(func() { execCommandContext, openCodeVersionCommand = prevExec, prevVersion })
	openCodeVersionCommand = func(context.Context, string) ([]byte, error) {
		return []byte("opencode v2.3.0"), nil
	}
	execCommandContext = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		t.Fatalf("execCommandContext called with bad format model: %s %v", name, args)
		return exec.CommandContext(ctx, "/nonexistent")
	}

	root := t.TempDir()
	_, err := StartOpenCodeServerIn(context.Background(), root, []string{"gpt-5.5"})
	if err == nil || !strings.Contains(err.Error(), "provider/model") {
		t.Errorf("StartOpenCodeServerIn with model without / = %v, want format error", err)
	}
}

func TestRunnerValidateModelsBootsAndClosesItsServe(t *testing.T) {
	t.Setenv(platform.DisableExternalRunnersEnv, "")
	useFakeServe(t, "lease")

	root := t.TempDir()
	r := &runner{cfg: store.Config{RuntimeRoot: root}}

	ctx := context.Background()
	// ValidateModels with a valid model (fake serves openai/gpt-5.5): should succeed and close
	if err := r.ValidateModels(ctx, "openai/gpt-5.5"); err != nil {
		t.Fatalf("ValidateModels(openai/gpt-5.5) = %v, want nil", err)
	}

	// Serve dir should be empty afterwards (Close cleaned up)
	serveDir := filepath.Join(root, "opencode-serve")
	entries, err := os.ReadDir(serveDir)
	if err != nil && !os.IsNotExist(err) {
		t.Fatalf("reading serve dir: %v", err)
	}
	if len(entries) > 0 {
		t.Errorf("serve dir has %d entries after ValidateModels, want 0", len(entries))
	}

	// ValidateModels with an unavailable model: should error and still close
	err = r.ValidateModels(ctx, "opencode/crouton-free")
	if err == nil {
		t.Fatalf("ValidateModels(opencode/crouton-free) = nil, want error")
	}

	// Serve dir should still be empty (Close on failure path)
	entries, err = os.ReadDir(serveDir)
	if err != nil && !os.IsNotExist(err) {
		t.Fatalf("reading serve dir after error: %v", err)
	}
	if len(entries) > 0 {
		t.Errorf("serve dir has %d entries after ValidateModels error, want 0", len(entries))
	}
}

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
