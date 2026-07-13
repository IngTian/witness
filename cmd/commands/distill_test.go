package commands

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/IngTian/witness/internal/store"
)

// --all means the ENTIRE backlog; combining it with a time bound is contradictory,
// so cmdDistillBackfill rejects it before doing any work. (A bounded backfill is
// just `distill start --since ...`, the background path.)
func TestDistillBackfillRejectsTimeBounds(t *testing.T) {
	for _, tc := range []struct{ since, until string }{
		{"7d", ""},
		{"", "2026-07-01"},
		{"2026-06-01", "2026-07-01"},
	} {
		err := cmdDistillBackfill(true, tc.since, tc.until)
		if err == nil {
			t.Fatalf("--all with since=%q until=%q should error", tc.since, tc.until)
		}
		if !strings.Contains(err.Error(), "cannot be combined") {
			t.Fatalf("unexpected error for since=%q until=%q: %v", tc.since, tc.until, err)
		}
	}
}

// Review #2: `--all` must NOT report success when the backlog was not drained.
// With pending L0 but no embedding model available (the test env has none), the
// worker can't mine, so work stays pending — and cmdDistillBackfill must surface
// that as a non-nil error ("backfill incomplete"), not print "complete" + exit 0.
func TestDistillBackfillFailsWhenWorkRemains(t *testing.T) {
	t.Setenv("WITNESS_HOME", filepath.Join(t.TempDir(), "witness"))
	// Lenses load from the repo's prompts/ (as other worker tests do); the embedder
	// points at an empty dir so embed.New fails (model not ready) — the drain then
	// can't mine and work stays pending, which is exactly what we're asserting on.
	t.Setenv("WITNESS_PROMPTS", filepath.Join("..", "..", "prompts"))
	t.Setenv("WITNESS_ASSETS", t.TempDir())
	st, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	// Leave a pending session in L0 (no model → it can never be distilled here).
	if err := st.AppendRaw(store.RawRecord{Session: "s", Seq: 0, Role: "user", Text: "hi"}); err != nil {
		t.Fatal(err)
	}
	st.Close()

	err = cmdDistillBackfill(true, "", "")
	if err == nil {
		t.Fatal("--all with undistillable pending work must return an error, not report success")
	}
	if !strings.Contains(err.Error(), "incomplete") {
		t.Fatalf("expected an 'incomplete' error, got: %v", err)
	}
}
