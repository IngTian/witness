package lens

import (
	"os"
	"path/filepath"
	"testing"
)

// TestLoadDefaultFromEmbed is the regression guard for the "prompts/ not found"
// bug: a standalone binary with NO on-disk prompts dir and none of the override
// env vars must still load the built-in prompts from the embedded FS. Before
// prompts were embedded, this path fell through to a nonexistent ./prompts and
// failed for any downloaded exe.
func TestLoadDefaultFromEmbed(t *testing.T) {
	// Scrub every override so promptsDirOverride() returns "" and readPrompt
	// falls back to the embed. t.Chdir would also matter (cwd probe), but the
	// override checks run first and short-circuit.
	t.Setenv("WITNESS_PROMPTS", "")
	os.Unsetenv("WITNESS_PROMPTS")
	t.Setenv("CLAUDE_PLUGIN_ROOT", "")
	os.Unsetenv("CLAUDE_PLUGIN_ROOT")

	l, err := LoadDefault()
	if err != nil {
		t.Fatalf("LoadDefault from embed: %v", err)
	}
	if l.Extract == "" || l.Review == "" {
		t.Fatalf("embedded default lens has empty prompts: extract=%d review=%d", len(l.Extract), len(l.Review))
	}

	lensP, unifiedP, err := LoadSummarizePrompts()
	if err != nil {
		t.Fatalf("LoadSummarizePrompts from embed: %v", err)
	}
	if lensP == "" || unifiedP == "" {
		t.Fatal("embedded summarize prompts are empty")
	}
}

// TestPromptsDirOverrideWins proves an on-disk prompts dir takes precedence over
// the embed, so a checkout/plugin install can still customize prompts and
// WITNESS_PROMPTS remains a working escape hatch.
func TestPromptsDirOverrideWins(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "default"), 0o755); err != nil {
		t.Fatal(err)
	}
	const marker = "OVERRIDE-EXTRACT"
	if err := os.WriteFile(filepath.Join(dir, "default", "extract.md"), []byte(marker), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "default", "review.md"), []byte("r"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("WITNESS_PROMPTS", dir)

	l, err := LoadDefault()
	if err != nil {
		t.Fatalf("LoadDefault with override: %v", err)
	}
	if l.Extract != marker {
		t.Errorf("override not used: got %q, want %q (embed leaked through)", l.Extract, marker)
	}
}
