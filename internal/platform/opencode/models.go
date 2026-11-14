package opencode

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// requireModels refuses an empty model name (an unnamed stage) and bad formats (no provider/model
// slash): OpenCode's default model (opencode/big-pickle with free models on) may train on inputs,
// so witness never lets OpenCode choose. It checks format before any spawn.
func requireModels(models []string) error {
	for _, m := range models {
		if _, err := splitOpenCodeModel(m); err != nil {
			return err
		}
	}
	return nil
}

// validateModels ensures configured OpenCode model names are available from the private serve. It
// fetches GET /api/model once, builds the available set (provider/id, plus provider/id#variant for
// every variant), and fails on the first model not in that set with unavailableModel.
func (s *OpenCodeServer) validateModels(ctx context.Context, models []string) error {
	data, err := s.doJSON(ctx, http.MethodGet, "/api/model", nil, http.StatusOK)
	if err != nil {
		return err
	}

	var result struct {
		Data []struct {
			ProviderID string `json:"providerID"`
			ID         string `json:"id"`
			Variants   []struct {
				ID string `json:"id"`
			} `json:"variants"`
		} `json:"data"`
	}
	if err := json.Unmarshal(data, &result); err != nil {
		return fmt.Errorf("opencode /api/model decode: %w", err)
	}

	// Build available set: provider/id, plus provider/id#variant for every variant
	available := map[string]bool{}
	for _, m := range result.Data {
		full := m.ProviderID + "/" + m.ID
		available[full] = true
		for _, v := range m.Variants {
			available[full+"#"+v.ID] = true
		}
	}

	// Check each model; group by provider for hints
	byProvider := map[string][]string{}
	for _, data := range result.Data {
		byProvider[data.ProviderID] = append(byProvider[data.ProviderID], data.ProviderID+"/"+data.ID)
	}

	for _, model := range models {
		model = strings.TrimSpace(model)
		if model == "" {
			continue
		}
		if !available[model] {
			provider, _, _ := strings.Cut(model, "/")
			return s.unavailableModel(ctx, model, byProvider[provider])
		}
	}
	return nil
}

// unavailableModel returns an error for a model not in the serve's catalog. For opencode/*
// models, the hint says free model ids are retired often and points to `opencode models`. For other
// providers, it names the three credential kinds the runner can use, plus this provider's env var
// names when providerEnvVars returns any.
func (s *OpenCodeServer) unavailableModel(ctx context.Context, model string, available []string) error {
	provider, _, _ := strings.Cut(model, "/")
	var hint string
	if provider == "opencode" {
		hint = "`opencode models` lists the free ids available now"
	} else {
		envVars := s.providerEnvVars(ctx, provider)
		parts := []string{"the runner can use free opencode/* models, API keys exported as environment variables"}
		if len(envVars) > 0 {
			parts[0] += fmt.Sprintf(" (for %s: %s)", provider, strings.Join(envVars, ", "))
		}
		parts = append(parts, ", and ambient cloud credentials such as AWS_PROFILE/AWS_REGION for Amazon Bedrock;")
		parts = append(parts, " it never uses logins saved with `opencode auth login`, ChatGPT or other OAuth sign-ins, or providers defined in your opencode.json")
		hint = strings.Join(parts, "")
	}
	return fmt.Errorf("opencode model %q is not available%s", model, modelHint(available)+"; "+hint)
}

// providerEnvVars calls GET /api/integration/<provider> and collects data.methods[type=env].names.
// It returns nil on any error (the provider might not have an integration route, or the route might
// be missing; the unavailableModel hint survives this and still names the three credential kinds).
func (s *OpenCodeServer) providerEnvVars(ctx context.Context, provider string) []string {
	data, err := s.doJSON(ctx, http.MethodGet, "/api/integration/"+provider, nil, http.StatusOK)
	if err != nil {
		return nil
	}

	var result struct {
		Data struct {
			Methods []struct {
				Type  string   `json:"type"`
				Names []string `json:"names"`
			} `json:"methods"`
		} `json:"data"`
	}
	if err := json.Unmarshal(data, &result); err != nil {
		return nil
	}

	for _, m := range result.Data.Methods {
		if m.Type == "env" {
			return m.Names
		}
	}
	return nil
}

// removeV1Runtime removes the v1 artifacts from <root>: a stale secret witness copied
// (xdg/opencode/auth.json), plus a v1 DB and pid file nothing reads (opencode.db, opencode.db-wal,
// opencode.db-shm, .witness-serve.pid). It keeps opencode-v2.db and everything else.
func removeV1Runtime(runtimeRoot string) {
	_ = os.RemoveAll(filepath.Join(runtimeRoot, "xdg"))
	_ = os.Remove(filepath.Join(runtimeRoot, "opencode.db"))
	_ = os.Remove(filepath.Join(runtimeRoot, "opencode.db-wal"))
	_ = os.Remove(filepath.Join(runtimeRoot, "opencode.db-shm"))
	_ = os.Remove(filepath.Join(runtimeRoot, ".witness-serve.pid"))
}
