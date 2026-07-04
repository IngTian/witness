package commands

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	opencodeplugin "github.com/IngTian/claude-witness/internal/runtimes/opencode/plugin"
	"github.com/IngTian/claude-witness/internal/store"
	"github.com/spf13/cobra"
)

func newInstallCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "install <claude|opencode>",
		Short: "Install witness integrations.",
		Long:  "Install the Claude Code integration (hooks + MCP) or the OpenCode integration (plugin + MCP). The target is required so install always binds the matching distillation runtime.",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			return cmdInstall(args)
		},
	}
}

func newUninstallCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "uninstall <claude|opencode>",
		Short: "Remove witness integrations without deleting data.",
		Long:  "Remove the Claude Code or OpenCode integration. The witness data store and config are left untouched.",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			return cmdUninstall(args)
		},
	}
}

// hookCmd / hookEntry mirror the settings.json hooks schema for the entries we own.
type hookCmd struct {
	Type    string `json:"type"`
	Command string `json:"command"`
	Async   bool   `json:"async,omitempty"`
}
type hookEntry struct {
	Matcher string    `json:"matcher,omitempty"`
	Hooks   []hookCmd `json:"hooks"`
}

// shellQuote single-quotes a path for safe use in a shell-executed hook command,
// POSIX-escaping any embedded single quote (close the quote, backslash-escape the
// quote, reopen). Claude Code runs hook commands through a shell, so an absolute
// path containing a space or shell metacharacter (e.g. ~/My Projects/...) would
// otherwise word-split and the hook would silently fail. The plugin path
// (hooks/hooks.json) uses double quotes because it needs ${CLAUDE_PLUGIN_ROOT}
// expanded; here the shim is an already-resolved literal path, so single quotes
// (no expansion) are safest.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// witnessHookSpecs is the canonical hook wiring, pointing at the given shim. Kept
// in lockstep with hooks/hooks.json (the plugin-install path uses that file).
func witnessHookSpecs(shim string) []struct {
	Event string
	Entry hookEntry
} {
	q := shellQuote(shim)
	return []struct {
		Event string
		Entry hookEntry
	}{
		{"SessionStart", hookEntry{Matcher: "startup|clear|compact|resume", Hooks: []hookCmd{{Type: "command", Command: q + " session-start"}}}},
		{"UserPromptSubmit", hookEntry{Hooks: []hookCmd{{Type: "command", Command: q + " capture", Async: true}}}},
		{"Stop", hookEntry{Hooks: []hookCmd{{Type: "command", Command: q + " capture", Async: true}}}},
		{"SessionEnd", hookEntry{Hooks: []hookCmd{{Type: "command", Command: q + " session-end", Async: true}}}},
	}
}

// isWitnessEntry reports whether a parsed hook entry is one of ours (any command
// references witness.sh) — so re-install/uninstall can find and replace them.
func isWitnessEntry(e any) bool {
	m, ok := e.(map[string]any)
	if !ok {
		return false
	}
	hs, _ := m["hooks"].([]any)
	for _, h := range hs {
		hm, _ := h.(map[string]any)
		if c, _ := hm["command"].(string); strings.Contains(c, "witness.sh") {
			return true
		}
	}
	return false
}

// mergeWitnessHooks adds the witness hooks to a settings.json document, replacing
// any existing witness entries (idempotent) and preserving all other hooks and
// settings verbatim.
func mergeWitnessHooks(data []byte, shim string) ([]byte, error) {
	root := map[string]any{}
	if len(strings.TrimSpace(string(data))) > 0 {
		if err := json.Unmarshal(data, &root); err != nil {
			return nil, fmt.Errorf("parse settings: %w", err)
		}
	}
	hooks, _ := root["hooks"].(map[string]any)
	if hooks == nil {
		hooks = map[string]any{}
	}
	for _, spec := range witnessHookSpecs(shim) {
		existing, _ := hooks[spec.Event].([]any)
		kept := []any{}
		for _, e := range existing {
			if !isWitnessEntry(e) {
				kept = append(kept, e) // preserve foreign hooks
			}
		}
		kept = append(kept, spec.Entry)
		hooks[spec.Event] = kept
	}
	root["hooks"] = hooks
	return json.MarshalIndent(root, "", "  ")
}

// removeWitnessHooks strips our hook entries, leaving foreign hooks and other
// settings intact. Empty event arrays are dropped.
func removeWitnessHooks(data []byte) ([]byte, error) {
	root := map[string]any{}
	if len(strings.TrimSpace(string(data))) > 0 {
		if err := json.Unmarshal(data, &root); err != nil {
			return nil, fmt.Errorf("parse settings: %w", err)
		}
	}
	hooks, _ := root["hooks"].(map[string]any)
	for event, v := range hooks {
		entries, _ := v.([]any)
		kept := []any{}
		for _, e := range entries {
			if !isWitnessEntry(e) {
				kept = append(kept, e)
			}
		}
		if len(kept) == 0 {
			delete(hooks, event)
		} else {
			hooks[event] = kept
		}
	}
	if len(hooks) == 0 {
		delete(root, "hooks")
	}
	return json.MarshalIndent(root, "", "  ")
}

func claudeDir() string {
	if d := os.Getenv("CLAUDE_CONFIG_DIR"); d != "" {
		return d
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".claude")
}

func opencodeDir() string {
	if d := os.Getenv("OPENCODE_CONFIG_DIR"); d != "" {
		return d
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "opencode")
}

func opencodeConfigPath() string {
	if p := os.Getenv("OPENCODE_CONFIG"); strings.TrimSpace(p) != "" {
		return p
	}
	dir := opencodeDir()
	jsonPath := filepath.Join(dir, "opencode.json")
	if _, err := os.Stat(jsonPath); err == nil {
		return jsonPath
	}
	jsoncPath := filepath.Join(dir, "opencode.jsonc")
	if _, err := os.Stat(jsoncPath); err == nil {
		return jsoncPath
	}
	return jsonPath
}

// repoShim resolves <repo>/hooks/witness.sh from the running binary at <repo>/bin/.
func repoShim() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	repo := filepath.Dir(filepath.Dir(exe)) // bin/ -> repo
	shim := filepath.Join(repo, "hooks", "witness.sh")
	if _, err := os.Stat(shim); err != nil {
		return "", fmt.Errorf("shim not found at %s (run from a built working copy: make build): %w", shim, err)
	}
	return shim, nil
}

// cmdInstall wires witness into Claude Code or OpenCode. The target is required
// (enforced by cobra ExactArgs) so install always binds the matching distillation
// runtime into config.toml.
func cmdInstall(args []string) error {
	target := installTarget(args)
	switch target {
	case "claude":
		if err := cmdInstallClaude(); err != nil {
			return err
		}
	case "opencode":
		if err := cmdInstallOpenCode(); err != nil {
			return err
		}
	default:
		return fmt.Errorf("unknown install target %q (want claude or opencode)", target)
	}
	return bindRunner(target)
}

// bindRunner pins config.toml's runner field to the integration that was just
// wired, so distillation uses the same agent runtime the user installed for.
// store.Open() already laid down the full template if config was missing; here
// we only flip runner. Best-effort: a store open failure (rare; Open mkdirs) is
// logged but does not fail install, since hooks/plugin are already in place.
func bindRunner(target string) error {
	st, err := store.Open()
	if err != nil {
		fmt.Fprintf(os.Stderr, "witness: could not open store to set runner: %v\n", err)
		return nil
	}
	defer st.Close()
	if err := st.SetRunner(target); err != nil {
		return fmt.Errorf("write runner to config: %w", err)
	}
	fmt.Printf("runner set to %s in witness config.toml\n", target)
	return nil
}

func installTarget(args []string) string {
	return strings.ToLower(strings.TrimSpace(args[0]))
}

// cmdInstallClaude wires the witness hooks into the user's Claude settings.json
// and registers the MCP server, both idempotently.
func cmdInstallClaude() error {
	shim, err := repoShim()
	if err != nil {
		return err
	}
	settings := filepath.Join(claudeDir(), "settings.json")
	if err := os.MkdirAll(claudeDir(), 0o755); err != nil {
		return err
	}
	data, err := os.ReadFile(settings)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	merged, err := mergeWitnessHooks(data, shim)
	if err != nil {
		return err
	}
	if err := writeFileAtomic(settings, merged); err != nil {
		return err
	}
	fmt.Printf("hooks wired into %s\n", settings)

	// Register the MCP server (idempotent: skip if already present).
	if out, _ := exec.Command("claude", "mcp", "list").CombinedOutput(); !strings.Contains(string(out), "witness") {
		if err := exec.Command("claude", "mcp", "add", "-s", "user", "witness", shim, "mcp").Run(); err != nil {
			fmt.Fprintf(os.Stderr, "witness: could not register MCP server (is `claude` on PATH?): %v\n", err)
		} else {
			fmt.Println("MCP server 'witness' registered")
		}
	} else {
		fmt.Println("MCP server 'witness' already registered")
	}
	fmt.Println("done — restart Claude Code (or open /hooks) to load the hooks.")
	fmt.Println("note: witness collects everywhere; the profile is never injected into a session.")
	fmt.Println("      agents read it on demand via the witness MCP tools;")
	fmt.Println("      you can read it yourself with `witness profile`.")
	return nil
}

// cmdInstallOpenCode installs a global OpenCode plugin that mirrors completed
// OpenCode sessions into witness, and registers the same MCP server OpenCode-side.
func cmdInstallOpenCode() error {
	shim, err := repoShim()
	if err != nil {
		return err
	}
	dir := opencodeDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	config := opencodeConfigPath()
	data, err := os.ReadFile(config)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	merged, err := mergeOpenCodeMCP(data, shim)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(config), 0o755); err != nil {
		return err
	}

	plugins := filepath.Join(dir, "plugins")
	if err := os.MkdirAll(plugins, 0o755); err != nil {
		return err
	}
	pluginPath := filepath.Join(plugins, "claude-witness.js")
	if err := writeFileAtomic(pluginPath, []byte(opencodeplugin.Source(shim))); err != nil {
		return err
	}
	fmt.Printf("OpenCode plugin installed at %s\n", pluginPath)

	if err := writeFileAtomic(config, merged); err != nil {
		return err
	}
	fmt.Printf("OpenCode MCP server 'witness' registered in %s\n", config)
	fmt.Println("done — restart OpenCode so the plugin and MCP server load.")
	return nil
}

// cmdUninstall removes the witness hooks/plugin and MCP server (idempotent).
// Data, config, and the runner setting are left untouched.
func cmdUninstall(args []string) error {
	target := installTarget(args)
	switch target {
	case "claude":
		return cmdUninstallClaude()
	case "opencode":
		return cmdUninstallOpenCode()
	default:
		return fmt.Errorf("unknown uninstall target %q (want claude or opencode)", target)
	}
}

func cmdUninstallClaude() error {
	settings := filepath.Join(claudeDir(), "settings.json")
	if data, err := os.ReadFile(settings); err == nil {
		if cleaned, err := removeWitnessHooks(data); err == nil {
			_ = writeFileAtomic(settings, cleaned)
			fmt.Printf("hooks removed from %s\n", settings)
		}
	}
	_ = exec.Command("claude", "mcp", "remove", "witness").Run()
	fmt.Println("MCP server 'witness' removed (if it was present)")
	return nil
}

func cmdUninstallOpenCode() error {
	dir := opencodeDir()
	pluginPath := filepath.Join(dir, "plugins", "claude-witness.js")
	config := opencodeConfigPath()
	configUpdated := false
	if data, err := os.ReadFile(config); err == nil {
		cleaned, err := removeOpenCodeMCP(data)
		if err != nil {
			return err
		}
		if err := writeFileAtomic(config, cleaned); err != nil {
			return err
		}
		configUpdated = true
	} else if !os.IsNotExist(err) {
		return err
	}
	_ = os.Remove(pluginPath)
	fmt.Printf("OpenCode plugin removed from %s (if it was present)\n", pluginPath)
	if configUpdated {
		fmt.Println("OpenCode MCP server 'witness' removed from OpenCode config (if it was present)")
	} else {
		fmt.Println("OpenCode config not found; no MCP entry removed")
	}
	return nil
}

func mergeOpenCodeMCP(data []byte, shim string) ([]byte, error) {
	root, err := parseOpenCodeConfig(data)
	if err != nil {
		return nil, err
	}
	if root["$schema"] == nil {
		root["$schema"] = "https://opencode.ai/config.json"
	}
	mcp, _ := root["mcp"].(map[string]any)
	if mcp == nil {
		mcp = map[string]any{}
	}
	mcp["witness"] = map[string]any{
		"type":    "local",
		"command": []any{shim, "mcp"},
		"enabled": true,
	}
	root["mcp"] = mcp
	return json.MarshalIndent(root, "", "  ")
}

func removeOpenCodeMCP(data []byte) ([]byte, error) {
	root, err := parseOpenCodeConfig(data)
	if err != nil {
		return nil, err
	}
	mcp, _ := root["mcp"].(map[string]any)
	delete(mcp, "witness")
	if len(mcp) == 0 {
		delete(root, "mcp")
	} else {
		root["mcp"] = mcp
	}
	return json.MarshalIndent(root, "", "  ")
}

func parseOpenCodeConfig(data []byte) (map[string]any, error) {
	root := map[string]any{}
	if len(strings.TrimSpace(string(data))) == 0 {
		return root, nil
	}
	if err := json.Unmarshal(normalizeJSONC(data), &root); err != nil {
		return nil, fmt.Errorf("parse opencode config: %w", err)
	}
	return root, nil
}

func normalizeJSONC(data []byte) []byte {
	return removeTrailingCommas(stripJSONComments(data))
}

func stripJSONComments(data []byte) []byte {
	out := make([]byte, 0, len(data))
	inString := false
	escaped := false
	for i := 0; i < len(data); i++ {
		c := data[i]
		if inString {
			out = append(out, c)
			if escaped {
				escaped = false
			} else if c == '\\' {
				escaped = true
			} else if c == '"' {
				inString = false
			}
			continue
		}
		if c == '"' {
			inString = true
			out = append(out, c)
			continue
		}
		if c == '/' && i+1 < len(data) && data[i+1] == '/' {
			for i < len(data) && data[i] != '\n' {
				i++
			}
			if i < len(data) {
				out = append(out, data[i])
			}
			continue
		}
		if c == '/' && i+1 < len(data) && data[i+1] == '*' {
			i += 2
			for i < len(data)-1 && !(data[i] == '*' && data[i+1] == '/') {
				if data[i] == '\n' {
					out = append(out, '\n')
				}
				i++
			}
			i++
			continue
		}
		out = append(out, c)
	}
	return out
}

func removeTrailingCommas(data []byte) []byte {
	out := make([]byte, 0, len(data))
	inString := false
	escaped := false
	for i := 0; i < len(data); i++ {
		c := data[i]
		if inString {
			out = append(out, c)
			if escaped {
				escaped = false
			} else if c == '\\' {
				escaped = true
			} else if c == '"' {
				inString = false
			}
			continue
		}
		if c == '"' {
			inString = true
			out = append(out, c)
			continue
		}
		if c == ',' {
			j := i + 1
			for j < len(data) && isJSONWhitespace(data[j]) {
				j++
			}
			if j < len(data) && (data[j] == '}' || data[j] == ']') {
				continue
			}
		}
		out = append(out, c)
	}
	return out
}

func isJSONWhitespace(c byte) bool {
	return c == ' ' || c == '\n' || c == '\r' || c == '\t'
}

// writeFileAtomic writes via temp + rename so a crash can't leave a half-written
// settings.json (which would silently disable ALL of the user's settings).
func writeFileAtomic(path string, data []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
