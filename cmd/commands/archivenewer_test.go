package commands

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/IngTian/witness/internal/store"
)

// bumpArchiveSchema stamps the WITNESS_HOME archive one schema past this binary, as a newer
// witness would leave it, and returns this binary's schema.
func bumpArchiveSchema(t *testing.T) int {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(os.Getenv("WITNESS_HOME"), "witness.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var v int
	if err := db.QueryRow("PRAGMA user_version").Scan(&v); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(fmt.Sprintf("PRAGMA user_version=%d", v+1)); err != nil {
		t.Fatal(err)
	}
	return v
}

// Every distillation entry point must refuse an archive a NEWER witness wrote, before it opens a
// runner — so before any model call is paid for — and before anything destructive (backfill
// --fresh drops a lens's facets ahead of the re-mine). writeFacetsTx refusing on its own would
// still let review pay for every window and then fail the write, forever.
//
// "No runner opened" is the observable: the fake runner counts Open, and nothing can call a model
// through a runner set that was never opened.
func TestNewerArchiveRefusesEveryDistillationEntryPoint(t *testing.T) {
	compactFixture(t) // lens "math", runner "fakecompact", three facets
	t.Setenv("WITNESS_PROMPTS", filepath.Join("..", "..", "prompts"))
	bumpArchiveSchema(t)

	opens := atomic.LoadInt32(&leakFakeOpens)
	withStore := func(f func(*store.Store) error) func() error {
		return func() error {
			st, err := store.Open()
			if err != nil {
				return err
			}
			defer st.Close()
			return f(st)
		}
	}
	for _, c := range []struct {
		name string
		run  func() error
	}{
		{"worker run", func() error { _, err := runWorker(false); return err }},
		{"worker review --full", func() error { return cmdReviewFull(true) }},
		{"compact (+ claim pass)", func() error { return cmdCompact(true, "", func(string) bool { return true }) }},
		{"lens backfill --fresh", withStore(func(st *store.Store) error { return lensBackfill(st, "math", true, true) })},
		{"profile regeneration", withStore(func(st *store.Store) error { _, err := ensureProfileFresh(st); return err })},
	} {
		if err := c.run(); !errors.Is(err, store.ErrArchiveNewer) {
			t.Errorf("%s on a newer archive returned %v, want store.ErrArchiveNewer", c.name, err)
		}
	}
	if got := atomic.LoadInt32(&leakFakeOpens) - opens; got != 0 {
		t.Errorf("a runner was opened %d time(s) on a newer archive; the refusal must come before any model call", got)
	}
	if active, archived := compactCounts(t); active != 3 || archived != 0 {
		t.Errorf("the facets changed on a newer archive: %d active, %d archived (want 3, 0)", active, archived)
	}
}

// doctor shows both schema versions, and a newer archive fails it: nothing can distill there.
func TestDoctorShowsBothSchemaVersions(t *testing.T) {
	t.Setenv("WITNESS_HOME", t.TempDir())
	t.Setenv("WITNESS_PROMPTS", filepath.Join("..", "..", "prompts"))
	t.Setenv("WITNESS_ASSETS", t.TempDir()) // no embedder model: doctor skips the slow load
	st, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	st.Close()
	binary := bumpArchiveSchema(t)

	out, err := captureStdoutErr(t, func() error { return cmdDoctor(true) })
	if !errors.Is(err, store.ErrArchiveNewer) {
		t.Errorf("doctor --json on a newer archive returned %v, want store.ErrArchiveNewer", err)
	}
	var report struct {
		Archive struct {
			Schema       int `json:"schema_version"`
			BinarySchema int `json:"binary_schema_version"`
		} `json:"archive"`
	}
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("unparseable doctor --json: %v\n%s", err, out)
	}
	if report.Archive.Schema != binary+1 || report.Archive.BinarySchema != binary {
		t.Errorf("doctor --json schema = (%d, %d), want (%d, %d)",
			report.Archive.Schema, report.Archive.BinarySchema, binary+1, binary)
	}

	human, _ := captureStdoutErr(t, func() error { return cmdDoctor(false) })
	if want := fmt.Sprintf("v%d", binary+1); !strings.Contains(human, want) || !strings.Contains(human, "newer witness") {
		t.Errorf("doctor's report should show %s and say a newer witness wrote it:\n%s", want, human)
	}
}
