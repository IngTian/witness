package commands

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/IngTian/witness/internal/store"
)

func ingestHome(t *testing.T) {
	t.Helper()
	t.Setenv("WITNESS_HOME", t.TempDir())
	t.Setenv("WITNESS_PROMPTS", filepath.Join("..", "..", "prompts"))
}

// rawRows reads a session's L0 rows through a fresh store handle.
func rawRows(t *testing.T, session string) []store.RawRecord {
	t.Helper()
	st, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	recs, err := st.ReadRaw(session)
	if err != nil {
		t.Fatal(err)
	}
	return recs
}

// A record with NO id must still be idempotent across re-ingests.
//
// It used to append unconditionally — the code said "hash-only keys never match by id" — so
// re-ingesting the same id-less document appended it AGAIN every time: unbounded L0 growth
// for exactly the shape a "just pipe me documents" feed uses, where the caller supplies no
// ids at all. Measured before the fix: three ingests of one id-less record produced 3 raw
// rows and 3 identical stored keys. The observations mined from those duplicates then
// reinforce the same facet repeatedly, so the archive over-weights a document purely because
// it was ingested twice.
func TestIngestDoesNotReappendAnIDLessRecord(t *testing.T) {
	ingestHome(t)
	rec := `{"text":"the same document body","session":"docs"}` + "\n"

	n, _, err := cmdIngest(strings.NewReader(rec), true)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("first ingest should write the record, got %d", n)
	}
	for pass := 2; pass <= 4; pass++ {
		n, _, err := cmdIngest(strings.NewReader(rec), true)
		if err != nil {
			t.Fatalf("pass %d: %v", pass, err)
		}
		if n != 0 {
			t.Errorf("pass %d re-ingested %d record(s): an unchanged id-less record must be a no-op", pass, n)
		}
	}
	if rows := rawRows(t, "file:docs"); len(rows) != 1 {
		t.Fatalf("L0 grew to %d rows from re-ingesting ONE id-less record", len(rows))
	}
	// The stored key list must not accumulate duplicates either — it is the dedup state.
	st, _ := store.Open()
	keys := st.MetaString("file_import_keys:file:docs")
	st.Close()
	if strings.Count(keys, "h:") != 1 {
		t.Errorf("stored keys accumulated duplicates: %s", keys)
	}
}

// A duplicate WITHIN a single batch must also collapse — oldKeySet cannot see it, because
// nothing is written until the batch completes.
func TestIngestCollapsesIDLessDuplicatesInOneBatch(t *testing.T) {
	ingestHome(t)
	body := `{"text":"same body","session":"docs"}` + "\n"
	batch := body + body + body
	n, _, err := cmdIngest(strings.NewReader(batch), true)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("three identical id-less records in one batch wrote %d rows, want 1", n)
	}
	if rows := rawRows(t, "file:docs"); len(rows) != 1 {
		t.Errorf("L0 has %d rows, want 1", len(rows))
	}
}

// DIFFERENT id-less bodies must all be kept: dedup is on content, not on "has no id".
func TestIngestKeepsDistinctIDLessRecords(t *testing.T) {
	ingestHome(t)
	var b strings.Builder
	for i := 0; i < 4; i++ {
		fmt.Fprintf(&b, `{"text":"body number %d","session":"docs"}`+"\n", i)
	}
	n, _, err := cmdIngest(strings.NewReader(b.String()), true)
	if err != nil {
		t.Fatal(err)
	}
	if n != 4 {
		t.Fatalf("4 distinct id-less records wrote %d rows", n)
	}
	// And CHANGED text still appends: with no id there is nothing to identify it as an edit
	// OF a prior record rather than a new document.
	if _, _, err := cmdIngest(strings.NewReader(`{"text":"a fifth, new body","session":"docs"}`+"\n"), true); err != nil {
		t.Fatal(err)
	}
	if rows := rawRows(t, "file:docs"); len(rows) != 5 {
		t.Errorf("want 5 rows, got %d", len(rows))
	}
}

// The id-ful path must be untouched: same id + same text is a no-op, same id + edited text
// UPDATES in place rather than appending.
func TestIngestIDPathStillDedupsAndUpdatesInPlace(t *testing.T) {
	ingestHome(t)
	if _, _, err := cmdIngest(strings.NewReader(`{"text":"v1","id":"doc-1","session":"s"}`+"\n"), true); err != nil {
		t.Fatal(err)
	}
	n, _, err := cmdIngest(strings.NewReader(`{"text":"v1","id":"doc-1","session":"s"}`+"\n"), true)
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("an unchanged id-ful record re-ingested %d times", n)
	}
	if _, _, err := cmdIngest(strings.NewReader(`{"text":"v2 edited","id":"doc-1","session":"s"}`+"\n"), true); err != nil {
		t.Fatal(err)
	}
	rows := rawRows(t, "file:s")
	if len(rows) != 1 {
		t.Fatalf("an edit must update in place, not append: %d rows", len(rows))
	}
	if !strings.Contains(rows[0].Text, "v2 edited") {
		t.Errorf("the edit did not land: %q", rows[0].Text)
	}
}

// A partial update must NOT delete records the caller did not mention. This is the documented
// "TRUE MERGE/APPEND" contract, and it goes through the replace=true rebuild path — an audit
// finding claimed that path was destructive; it is not, and this pins that.
func TestIngestPartialUpdateKeepsUnmentionedRecords(t *testing.T) {
	ingestHome(t)
	first := `{"text":"doc A","id":"a","session":"s"}` + "\n" + `{"text":"doc B","id":"b","session":"s"}` + "\n"
	if _, _, err := cmdIngest(strings.NewReader(first), true); err != nil {
		t.Fatal(err)
	}
	// Mention ONLY b, edited.
	if _, _, err := cmdIngest(strings.NewReader(`{"text":"doc B EDITED","id":"b","session":"s"}`+"\n"), true); err != nil {
		t.Fatal(err)
	}
	rows := rawRows(t, "file:s")
	if len(rows) != 2 {
		t.Fatalf("a partial update changed the row count to %d; unmentioned records must survive", len(rows))
	}
	var sawA, sawEdited bool
	for _, r := range rows {
		if strings.Contains(r.Text, "doc A") {
			sawA = true
		}
		if strings.Contains(r.Text, "doc B EDITED") {
			sawEdited = true
		}
	}
	if !sawA {
		t.Error("the unmentioned record was deleted")
	}
	if !sawEdited {
		t.Error("the mentioned record was not updated")
	}
}

// Mixing id-ful and id-less records in one batch must work: each is deduped by its own rule.
func TestIngestMixedIDAndIDLessBatch(t *testing.T) {
	ingestHome(t)
	batch := `{"text":"with id","id":"x","session":"s"}` + "\n" + `{"text":"no id","session":"s"}` + "\n"
	n, _, err := cmdIngest(strings.NewReader(batch), true)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("mixed batch wrote %d rows, want 2", n)
	}
	// Re-ingest the identical batch: both must be no-ops.
	n, _, err = cmdIngest(strings.NewReader(batch), true)
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("re-ingesting an identical mixed batch wrote %d rows", n)
	}
	if rows := rawRows(t, "file:s"); len(rows) != 2 {
		t.Errorf("L0 has %d rows, want 2", len(rows))
	}
}
