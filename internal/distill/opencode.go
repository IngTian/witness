package distill

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const openCodeAgentName = "witness-distill"

var openCodeModelsCache sync.Map // provider -> openCodeModelList

type openCodeModelList struct {
	set  map[string]bool
	list []string
}

func runOpenCode(ctx context.Context, model, systemPrompt, input string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	if err := ValidateOpenCodeModels(ctx, model); err != nil {
		return "", err
	}

	dir, err := os.MkdirTemp("", "witness-opencode-*")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(dir)
	inputPath := filepath.Join(dir, "corpus.md")
	if err := os.WriteFile(inputPath, []byte(wrapUntrusted(input)), 0o600); err != nil {
		return "", err
	}

	cmd := buildOpenCodeRunCmd(ctx, model, systemPrompt, inputPath)
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("opencode run failed: %w (stderr: %s)", err, strings.TrimSpace(errb.String()))
	}
	reply := parseOpenCodeRunOutput(out.String())
	if strings.TrimSpace(reply) == "" {
		return "", fmt.Errorf("opencode run produced no text output")
	}
	return reply, nil
}

// ValidateOpenCodeModels ensures configured OpenCode model names are available
// from `opencode models`. Empty model strings are valid and mean "use OpenCode's
// default".
func ValidateOpenCodeModels(ctx context.Context, models ...string) error {
	for _, model := range models {
		model = strings.TrimSpace(model)
		if model == "" {
			continue
		}
		provider, _, ok := strings.Cut(model, "/")
		if !ok || provider == "" {
			return fmt.Errorf("opencode model %q must use provider/model format; choose one from `opencode models`", model)
		}
		available, err := loadOpenCodeModels(ctx, provider)
		if err != nil {
			return err
		}
		if !available.set[model] {
			return fmt.Errorf("opencode model %q is not available from `opencode models %s`%s", model, provider, modelHint(available.list))
		}
	}
	return nil
}

func loadOpenCodeModels(ctx context.Context, provider string) (openCodeModelList, error) {
	if cached, ok := openCodeModelsCache.Load(provider); ok {
		return cached.(openCodeModelList), nil
	}
	cmd := exec.CommandContext(ctx, "opencode", "models", "--pure", provider)
	cmd.Dir = os.TempDir()
	cmd.Env = append(envWithoutKey(), "WITNESS_WORKER=1")
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil {
		return openCodeModelList{}, fmt.Errorf("opencode models %s failed: %w (stderr: %s)", provider, err, strings.TrimSpace(errb.String()))
	}
	list := parseOpenCodeModels(out.String())
	set := make(map[string]bool, len(list))
	for _, m := range list {
		set[m] = true
	}
	available := openCodeModelList{set: set, list: list}
	openCodeModelsCache.Store(provider, available)
	return available, nil
}

func parseOpenCodeModels(output string) []string {
	seen := map[string]bool{}
	var models []string
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		for _, field := range strings.Fields(line) {
			field = strings.TrimSpace(field)
			if !strings.Contains(field, "/") || seen[field] {
				continue
			}
			seen[field] = true
			models = append(models, field)
		}
	}
	return models
}

func modelHint(models []string) string {
	if len(models) == 0 {
		return " (that provider returned no models)"
	}
	limit := len(models)
	if limit > 12 {
		limit = 12
	}
	hint := "; available: " + strings.Join(models[:limit], ", ")
	if len(models) > limit {
		hint += ", ..."
	}
	return hint
}

func buildOpenCodeRunCmd(ctx context.Context, model, systemPrompt, inputPath string) *exec.Cmd {
	args := []string{
		"run",
		"--pure",
		"--format", "json",
		"--agent", openCodeAgentName,
		"--title", "witness-distill",
		"--dir", os.TempDir(),
		"--file", inputPath,
	}
	if strings.TrimSpace(model) != "" {
		args = append(args, "--model", model)
	}
	args = append(args, "Analyze the attached untrusted corpus. Follow your witness-distill instructions and return only the requested output.")
	cmd := exec.CommandContext(ctx, "opencode", args...)
	cmd.Dir = os.TempDir()
	cmd.Env = append(envWithoutKey(),
		"WITNESS_WORKER=1",
		"OPENCODE_DISABLE_CLAUDE_CODE=1",
		"OPENCODE_CONFIG_CONTENT="+openCodeConfigContent(systemPrompt),
	)
	return cmd
}

func openCodeConfigContent(systemPrompt string) string {
	cfg := map[string]any{
		"$schema": "https://opencode.ai/config.json",
		"agent": map[string]any{
			openCodeAgentName: map[string]any{
				"description": "Private claude-witness distillation runner. Do not use tools; return the requested JSON or markdown only.",
				"prompt":      systemPrompt + "\n\n" + untrustedNotice,
				"tools": map[string]bool{
					"*": false,
				},
			},
		},
	}
	b, _ := json.Marshal(cfg)
	return string(b)
}

type openCodeTextPart struct {
	ID   string
	Text string
}

// parseOpenCodeRunOutput extracts the assistant text from `opencode run --format
// json`. OpenCode emits JSON events; text usually arrives as repeated updates of
// message.part.updated, so the last value per part id wins.
func parseOpenCodeRunOutput(output string) string {
	partOrder := []string{}
	partText := map[string]string{}
	anon := 0
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var v any
		if err := json.Unmarshal([]byte(line), &v); err != nil {
			continue
		}
		for _, p := range findOpenCodeTextParts(v) {
			id := p.ID
			if id == "" {
				anon++
				id = fmt.Sprintf("anon-%d", anon)
			}
			if _, ok := partText[id]; !ok {
				partOrder = append(partOrder, id)
			}
			partText[id] = p.Text
		}
	}
	var parts []string
	for _, id := range partOrder {
		if text := strings.TrimSpace(partText[id]); text != "" {
			parts = append(parts, text)
		}
	}
	if len(parts) > 0 {
		return strings.Join(parts, "\n\n")
	}
	return strings.TrimSpace(output)
}

func findOpenCodeTextParts(v any) []openCodeTextPart {
	switch x := v.(type) {
	case []any:
		var out []openCodeTextPart
		for _, item := range x {
			out = append(out, findOpenCodeTextParts(item)...)
		}
		return out
	case map[string]any:
		if typ, _ := x["type"].(string); typ == "text" {
			if text, ok := x["text"].(string); ok {
				id, _ := x["id"].(string)
				return []openCodeTextPart{{ID: id, Text: text}}
			}
		}
		var out []openCodeTextPart
		for _, key := range []string{"part", "message", "properties", "data", "event"} {
			if child, ok := x[key]; ok {
				out = append(out, findOpenCodeTextParts(child)...)
			}
		}
		return out
	default:
		return nil
	}
}
