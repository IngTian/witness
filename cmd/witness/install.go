package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

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

// cmdInstall wires the witness hooks into the user's settings.json and registers
// the MCP server, both idempotently. Pairs with the Makefile/install.sh which
// build the binary and fetch the model first.
func cmdInstall() error {
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

// cmdUninstall removes the witness hooks and MCP server (idempotent).
func cmdUninstall() error {
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

// writeFileAtomic writes via temp + rename so a crash can't leave a half-written
// settings.json (which would silently disable ALL of the user's settings).
func writeFileAtomic(path string, data []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
