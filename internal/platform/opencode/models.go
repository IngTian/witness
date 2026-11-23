package opencode

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
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
// polls GET /api/model until every model is listed: from 2.4 the serve answers ready before its
// catalog loads, and the first reads list nothing for about a second (one read refused every
// model). When healthCheckBudget runs out it refuses the first model missing from the last list it
// read, with unavailableModel; with no list read at all, the last poll's error.
func (s *OpenCodeServer) validateModels(ctx context.Context, models []string) error {
	parent := ctx
	ctx, cancel := context.WithTimeout(ctx, healthCheckBudget)
	defer cancel()
	var missing string
	var byProvider map[string][]string // nil until a list is read
	var lastErr error
	for {
		available, ids, err := s.listModels(ctx)
		if err == nil {
			missing, byProvider = "", ids
			for _, model := range models {
				if model = strings.TrimSpace(model); model != "" && !available[model] {
					missing = model
					break
				}
			}
			if missing == "" {
				return nil
			}
		} else if ctx.Err() == nil || lastErr == nil { // a poll the budget cut off says only that
			lastErr = err
		}
		select {
		case <-ctx.Done():
			if parent.Err() != nil { // the caller gave up (worker stop, Ctrl-C): not a refusal
				return fmt.Errorf("stopped waiting for opencode's model list: %w", parent.Err())
			}
			if byProvider == nil {
				return lastErr
			}
			provider, _, _ := strings.Cut(missing, "/")
			return s.unavailableModel(parent, missing, byProvider[provider])
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// listModels reads one GET /api/model snapshot: the available set (provider/id, plus
// provider/id#variant for every variant) and each provider's ids, for the refusal's hint.
func (s *OpenCodeServer) listModels(ctx context.Context) (map[string]bool, map[string][]string, error) {
	data, err := s.doJSON(ctx, http.MethodGet, "/api/model", nil, http.StatusOK)
	if err != nil {
		return nil, nil, err
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
		return nil, nil, fmt.Errorf("opencode /api/model decode: %w", err)
	}
	available := map[string]bool{}
	byProvider := map[string][]string{}
	for _, m := range result.Data {
		full := m.ProviderID + "/" + m.ID
		available[full] = true
		byProvider[m.ProviderID] = append(byProvider[m.ProviderID], full)
		for _, v := range m.Variants {
			available[full+"#"+v.ID] = true
		}
	}
	return available, byProvider, nil
}

// unavailableModel returns an error for a model not in the serve's catalog. For opencode/*
// models, the hint points to `opencode models`, which lists the free ids available now. For other
// providers, it names the three credential kinds the runner can use, plus this provider's env var
// names when providerEnvVars returns any.
func (s *OpenCodeServer) unavailableModel(ctx context.Context, model string, available []string) error {
	provider, _, _ := strings.Cut(model, "/")
	var hint string
	if provider == "opencode" {
		hint = "`opencode models` lists the free ids available now"
	} else {
		var forProvider string
		if envVars := s.providerEnvVars(ctx, provider); len(envVars) > 0 {
			forProvider = fmt.Sprintf(" (for %s: %s)", provider, strings.Join(envVars, ", "))
		}
		hint = fmt.Sprintf("the runner can use free opencode/* models, API keys exported as environment variables%s, "+
			"and ambient cloud credentials such as AWS_PROFILE/AWS_REGION for Amazon Bedrock; it never uses logins "+
			"saved with `opencode auth login`, ChatGPT or other OAuth sign-ins, or providers defined in your opencode.json",
			forProvider)
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

// removeRetiredRuntime removes the runtime state no current witness reads from <root>: the v1
// artifacts — a stale secret witness copied (xdg/opencode/auth.json), plus a v1 DB and pid file
// (opencode.db, opencode.db-wal, opencode.db-shm, .witness-serve.pid) — and opencode-native, the
// removed native-session cache: its manifests, plus any v0.13-era .snapshot.json exports and the
// source-* copies of the user's opencode.db they were exported from. Its scratch sessions stay in
// opencode-v2.db, unseen: reaping them needs the removed machinery. It keeps opencode-v2.db and
// everything else. An empty root removes nothing: joined to "", those paths are the cwd's, and the
// worker's cwd is a user repo.
func removeRetiredRuntime(runtimeRoot string) {
	if runtimeRoot == "" {
		return
	}
	_ = os.RemoveAll(filepath.Join(runtimeRoot, "xdg"))
	_ = os.Remove(filepath.Join(runtimeRoot, "opencode.db"))
	_ = os.Remove(filepath.Join(runtimeRoot, "opencode.db-wal"))
	_ = os.Remove(filepath.Join(runtimeRoot, "opencode.db-shm"))
	_ = os.Remove(filepath.Join(runtimeRoot, ".witness-serve.pid"))
	_ = os.RemoveAll(filepath.Join(runtimeRoot, "opencode-native"))
}
