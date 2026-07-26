package commands

import (
	"path/filepath"
	"testing"

	"github.com/IngTian/witness/internal/lens"
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
func TestConfigSetLensOverrideVsDefault(t *testing.T) {
	home := t.TempDir()
	t.Setenv("WITNESS_HOME", home)
	t.Setenv("WITNESS_PROMPTS", filepath.Join("..", "..", "prompts"))
	st, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	// default scope
	if err := configApplySet(st, "mine_model", "haiku", ""); err != nil {
		t.Fatal(err)
	}
	if got := st.LoadConfig().TriageModel; got != "haiku" {
		t.Fatalf("default mine_model → TriageModel = %q, want haiku", got)
	}
	// lens scope (default lens is auto-seeded on Open)
	if err := configApplySet(st, "review_model", "opus", "default"); err != nil {
		t.Fatal(err)
	}
	l, err := lens.LoadRegistered("default", st.LensesDir())
	if err != nil {
		t.Fatal(err)
	}
	if l.ReviewModel != "opus" {
		t.Fatalf("lens review_model = %q, want opus", l.ReviewModel)
	}
}
