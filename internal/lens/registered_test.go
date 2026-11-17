package lens

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// writeLensDir lays down a lens directory in the new format (issue #75): an optional
// lens.json (nil skips it) plus extract.md and review.md. Returns the lens dir.
func writeLensDir(t *testing.T, root, name string, cfg *LensConfig, extract, review string) string {
	t.Helper()
	dir := filepath.Join(root, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if cfg != nil {
		data, err := json.Marshal(cfg)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, ConfigFile), data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, ExtractFile), []byte(extract), 0o644); err != nil {
		t.Fatal(err)
	}
	if review != "" {
		if err := os.WriteFile(filepath.Join(dir, ReviewFile), []byte(review), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestLoadRegistered(t *testing.T) {
	dir := t.TempDir()
	writeLensDir(t, dir, "math",
		&LensConfig{Dimensions: []string{"speed", "proof"}, ExtractModel: "openai/gpt-5.5-mini"},
		"mine growth", "synthesize")

	l, err := LoadRegistered("math", dir)
	if err != nil || l == nil {
		t.Fatalf("LoadRegistered: l=%v err=%v", l, err)
	}
	if l.Name != "math" || l.Extract == "" || l.Review == "" || len(l.Dimensions) != 2 {
		t.Fatalf("loaded lens wrong: %+v", l)
	}
	if l.ExtractModel != "openai/gpt-5.5-mini" {
		t.Fatalf("per-lens extract_model not loaded from lens.json, got %q", l.ExtractModel)
	}
	if l.ReviewModel != "" {
		t.Fatalf("unset review_model should be empty (ride the default), got %q", l.ReviewModel)
	}

	if _, err := LoadRegistered("missing", dir); err == nil {
		t.Fatalf("expected error for unregistered lens")
	}
}

// lens.json is OPTIONAL: a directory with only the two prompt files loads, with the
// name falling back to the directory name and no per-lens models.
func TestLoadRegisteredWithoutConfigJSON(t *testing.T) {
	dir := t.TempDir()
	writeLensDir(t, dir, "codereview", nil, "mine code review", "synth")
	l, err := LoadRegistered("codereview", dir)
	if err != nil {
		t.Fatalf("LoadRegistered without lens.json: %v", err)
	}
	if l.Name != "codereview" {
		t.Fatalf("name should fall back to the dir name, got %q", l.Name)
	}
	if l.ExtractModel != "" || l.ReviewModel != "" {
		t.Fatalf("a lens with no lens.json must have no per-lens models: %+v", l)
	}
}

// An empty extract.md is a hard error — the mining prompt is required, and a missing/
// empty prompt is a loud failure now (never a silently-empty section, the failure the
// old sectioned split-parser could produce).
func TestLoadRegisteredRequiresNonEmptyExtract(t *testing.T) {
	dir := t.TempDir()
	writeLensDir(t, dir, "empty", &LensConfig{}, "   \n", "rev")
	if _, err := LoadRegistered("empty", dir); err == nil {
		t.Fatalf("an empty extract.md must error (the mining prompt is required)")
	}
}

// A legacy directory holding ONLY the old single lens.md (pre-#75 sectioned format) is
// rejected with an actionable error, not silently mis-loaded.
func TestLoadRegisteredRejectsLegacyLensMD(t *testing.T) {
	dir := t.TempDir()
	old := filepath.Join(dir, "legacy")
	if err := os.MkdirAll(old, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(old, "lens.md"),
		[]byte("# name: legacy\n## EXTRACT\nmine\n## REVIEW\nrev\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadRegistered("legacy", dir); err == nil {
		t.Fatalf("a legacy single-file lens.md must be rejected with a re-register hint")
	}
}

// A lens has one name, the one it is registered under: a `name` left in its lens.json is
// ignored, so it can neither rename the lens nor make it the reserved "unified" portrait. The
// registered name itself is still checked against the reserved one.
func TestLoadRegisteredIgnoresTheLensJSONName(t *testing.T) {
	dir := t.TempDir()
	for reg, cfg := range map[string]string{
		"corpus-example": `{"name":"regime","dimensions":["rates"]}`,
		"foo":            `{"name":"Unified"}`,
	} {
		writeLensDir(t, dir, reg, nil, "mine", "rev")
		if err := os.WriteFile(filepath.Join(dir, reg, ConfigFile), []byte(cfg), 0o644); err != nil {
			t.Fatal(err)
		}
		l, err := LoadRegistered(reg, dir)
		if err != nil || l.Name != reg {
			t.Fatalf("LoadRegistered(%q) = %+v, %v; want name %q", reg, l, err, reg)
		}
		if reg == "corpus-example" && !slices.Equal(l.Dimensions, []string{"rates"}) {
			t.Fatalf("the rest of lens.json must still load, dimensions = %v", l.Dimensions)
		}
	}
	writeLensDir(t, dir, "unified", nil, "mine", "rev")
	if _, err := LoadRegistered("unified", dir); err == nil || !strings.Contains(err.Error(), "reserved") {
		t.Fatalf("a lens registered under the reserved name must be refused at load, got %v", err)
	}
}
