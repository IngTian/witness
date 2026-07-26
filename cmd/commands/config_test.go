package commands

import (
	"path/filepath"
	"testing"

	"github.com/IngTian/witness/internal/store"
)

func TestCanonicalConfigKey(t *testing.T) {
	cases := map[string]string{
		"runner": "runner", "mine_model": "mine_model", "review_model": "review_model",
		"triage_model": "mine_model", "distill_model": "review_model", // legacy synonyms
		"extract_model": "mine_model",
		"nope":          "", "": "",
	}
	for in, want := range cases {
		if got := canonicalConfigKey(in); got != want {
			t.Errorf("canonicalConfigKey(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestConfigSetLensOverrideVsDefault uses the same openSeedTestStore helper pattern as defaultseed_test.go
// Strengthened to prove storage-key routing (not just end-to-end read-your-write):
// default scope mine_model→triage_model, review_model→distill_model; lens scope
// mine_model→extract, review_model→review. Also confirms legacy synonyms route the same.
func TestConfigSetLensOverrideVsDefault(t *testing.T) {
	home := t.TempDir()
	t.Setenv("WITNESS_HOME", home)
	t.Setenv("WITNESS_PROMPTS", filepath.Join("..", "..", "prompts"))
	st, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	// DEFAULT SCOPE: prove mine_model→triage_model AND review_model→distill_model.
	if err := configApplySet(st, "mine_model", "haiku", ""); err != nil {
		t.Fatal(err)
	}
	if err := configApplySet(st, "review_model", "opus-default", ""); err != nil {
		t.Fatal(err)
	}
	// Reopen/reload to prove it landed in the correct storage keys.
	st.Close()
	st, err = store.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cfg := st.LoadConfig()
	if cfg.TriageModel != "haiku" {
		t.Errorf("default mine_model → TriageModel = %q, want haiku", cfg.TriageModel)
	}
	if cfg.DistillModel != "opus-default" {
		t.Errorf("default review_model → DistillModel = %q, want opus-default", cfg.DistillModel)
	}

	// LEGACY SYNONYM: confirm triage_model canonicalizes to mine_model, then routes to TriageModel.
	// canonicalConfigKey is what the command layer uses before calling configApplySet, so test it.
	if canon := canonicalConfigKey("triage_model"); canon != "mine_model" {
		t.Errorf("canonicalConfigKey(triage_model) = %q, want mine_model", canon)
	}
	// Verify distill_model→review_model canonicalization too.
	if canon := canonicalConfigKey("distill_model"); canon != "review_model" {
		t.Errorf("canonicalConfigKey(distill_model) = %q, want review_model", canon)
	}

	// LENS SCOPE: prove mine_model→ExtractModel AND review_model→ReviewModel (phase routing).
	// Use the auto-seeded "default" lens.
	if err := configApplySet(st, "mine_model", "haiku-lens", "default"); err != nil {
		t.Fatal(err)
	}
	if err := configApplySet(st, "review_model", "opus-lens", "default"); err != nil {
		t.Fatal(err)
	}
	// Read the RAW lens.json to prove the storage-key routing (not just resolved LoadRegistered).
	rawCfg, err := configReadRawLensJSON(st, "default")
	if err != nil {
		t.Fatal(err)
	}
	if rawCfg.ExtractModel != "haiku-lens" {
		t.Errorf("lens mine_model → ExtractModel = %q, want haiku-lens", rawCfg.ExtractModel)
	}
	if rawCfg.ReviewModel != "opus-lens" {
		t.Errorf("lens review_model → ReviewModel = %q, want opus-lens", rawCfg.ReviewModel)
	}
}
