package commands

import (
	"strings"
	"testing"

	"github.com/IngTian/witness/internal/store"
)

// desyncKeys forges a stored key list LONGER than the session's raw rows, so a positional
// update's index points past the rows that actually exist.
//
// This is not a contrived state: the replace path exists precisely BECAUSE the key list and
// raw can desynchronize (rawCount != len(oldKeys)) — a `cleanup` reclaiming rows, an
// interrupted write, a crash between the two.
func desyncKeys(t *testing.T, session, keysJSON string) {
	t.Helper()
	st, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.SetMetaString("file_import_keys:"+session, keysJSON); err != nil {
		t.Fatal(err)
	}
}

// An update that cannot be placed positionally must not be reported as ingested.
//
// The bounds guard correctly refused to write past the existing rows, but the return value was
// len(updates)+len(appends) regardless — so `witness ingest` printed "1 ingested" for an edit
// it had silently thrown away. Reproduced before the fix: reported ingested=1 with the edited
// text nowhere in L0.
//
// Telling the user their document updated when it did not is the real damage: they move on and
// the archive keeps distilling the stale body. The record is now APPENDED instead of dropped —
// its old position is gone, so appending preserves the caller's text — and the count reflects
// what actually landed.
func TestIngestOutOfRangeUpdateIsPreservedAndCountedHonestly(t *testing.T) {
	ingestHome(t)
	if _, _, err := cmdIngest(strings.NewReader(`{"text":"body a","id":"a","session":"s"}`+"\n"), true); err != nil {
		t.Fatal(err)
	}
	// Claim five keys ("e" at index 4) while raw holds one row.
	desyncKeys(t, "file:s",
		`["a:68d3a6450cfc8466","x:1111111111111111","y:2222222222222222","z:3333333333333333","e:4444444444444444"]`)

	n, _, err := cmdIngest(strings.NewReader(`{"text":"body e EDITED","id":"e","session":"s"}`+"\n"), true)
	if err != nil {
		t.Fatal(err)
	}
	rows := rawRows(t, "file:s")
	landed := false
	for _, r := range rows {
		if strings.Contains(r.Text, "body e EDITED") {
			landed = true
		}
	}
	if !landed {
		t.Errorf("the record was DISCARDED (reported %d ingested); rows=%+v", n, rows)
	}
	if n != 1 {
		t.Errorf("reported %d ingested, want exactly the 1 record that landed", n)
	}
	// The original record must survive untouched.
	var sawA bool
	for _, r := range rows {
		if strings.Contains(r.Text, "body a") {
			sawA = true
		}
	}
	if !sawA {
		t.Error("the pre-existing record was lost")
	}
}

// The count must never exceed what is in L0, even across a desync — that is the property the
// user reads as "N ingested".
func TestIngestReportedCountMatchesWhatLanded(t *testing.T) {
	ingestHome(t)
	if _, _, err := cmdIngest(strings.NewReader(`{"text":"body a","id":"a","session":"s"}`+"\n"), true); err != nil {
		t.Fatal(err)
	}
	before := len(rawRows(t, "file:s"))
	desyncKeys(t, "file:s", `["a:68d3a6450cfc8466","p:5555555555555555","q:6666666666666666"]`)

	batch := `{"text":"edit p","id":"p","session":"s"}` + "\n" +
		`{"text":"edit q","id":"q","session":"s"}` + "\n" +
		`{"text":"brand new","id":"new","session":"s"}` + "\n"
	n, _, err := cmdIngest(strings.NewReader(batch), true)
	if err != nil {
		t.Fatal(err)
	}
	after := len(rawRows(t, "file:s"))
	if grew := after - before; n != grew {
		t.Errorf("reported %d ingested but L0 grew by %d (before=%d after=%d)", n, grew, before, after)
	}
}

// A normal in-place update (no desync) must still be counted and still update in place —
// the honest-count change must not turn ordinary edits into appends.
func TestIngestInRangeUpdateStillUpdatesInPlaceAndCounts(t *testing.T) {
	ingestHome(t)
	first := `{"text":"body a","id":"a","session":"s"}` + "\n" + `{"text":"body b","id":"b","session":"s"}` + "\n"
	if _, _, err := cmdIngest(strings.NewReader(first), true); err != nil {
		t.Fatal(err)
	}
	n, _, err := cmdIngest(strings.NewReader(`{"text":"body a EDITED","id":"a","session":"s"}`+"\n"), true)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("an in-place update reported %d, want 1", n)
	}
	rows := rawRows(t, "file:s")
	if len(rows) != 2 {
		t.Fatalf("an in-place update changed the row count to %d, want 2", len(rows))
	}
	if !strings.Contains(rows[0].Text, "body a EDITED") {
		t.Errorf("the update did not land at its original position: %q", rows[0].Text)
	}
	if !strings.Contains(rows[1].Text, "body b") {
		t.Errorf("the sibling record was disturbed: %q", rows[1].Text)
	}
}
