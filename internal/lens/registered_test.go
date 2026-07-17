package lens

import (
	"encoding/json"
	"os"
	"path/filepath"
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
		&LensConfig{Name: "math", Dimensions: []string{"speed", "proof"}, ExtractModel: "openai/gpt-5.5-mini"},
		"mine growth", "synthesize")

	l, err := LoadRegistered("math", dir)
	if err != nil || l == nil {
		t.Fatalf("LoadRegistered: l=%v err=%v", l, err)
	}
	if l.Name != "math" || l.BuiltIn || l.Extract == "" || l.Review == "" || len(l.Dimensions) != 2 {
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
	writeLensDir(t, dir, "empty", &LensConfig{Name: "empty"}, "   \n", "rev")
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

// The reserved-name guard's ultimate backstop: a lens registered under an innocent
// directory name whose lens.json `name` resolves to a reserved identity ("default" or
// "unified") must be REJECTED at load — else it would impersonate the always-on built-in
// / the unified summary and collide on the shared lens key. RegisterLens/EnableLens guard
// the registry NAME, but only the resolved json name is known here, so this is where the
// impersonation is caught. Case variants too (the reserved-name check folds case).
func TestLoadRegisteredRejectsReservedJSONName(t *testing.T) {
	for _, reserved := range []string{"default", "unified", "Default", "UNIFIED"} {
		t.Run(reserved, func(t *testing.T) {
			dir := t.TempDir()
			// Registered under an innocent dir name "foo", but lens.json claims a reserved
			// identity — the exact bypass the registry-name guard can't see.
			writeLensDir(t, dir, "foo", &LensConfig{Name: reserved}, "mine", "rev")
			if _, err := LoadRegistered("foo", dir); err == nil {
				t.Fatalf("a lens whose lens.json name resolves to reserved %q must be rejected at load", reserved)
			}
		})
	}
}
