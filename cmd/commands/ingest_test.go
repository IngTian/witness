package commands

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/IngTian/witness/internal/store"
)

func TestParseNDJSONSkipsBadLines(t *testing.T) {
	in := `{"text":"a","id":"1"}
not json
{"id":"2"}
{"text":"b","id":"3","session":"s"}
`
	recs, skipped := parseNDJSON(strings.NewReader(in))
	if len(recs) != 2 { // "a" and "b"; "not json" + text-less {"id":"2"} skipped
		t.Fatalf("parsed %d records, want 2 (%+v)", len(recs), recs)
	}
	if skipped != 2 {
		t.Fatalf("skipped = %d, want 2", skipped)
	}
}

func TestRecordKeyIdentityVsFallback(t *testing.T) {
	// Same id, different text → different key (detects edit).
	if recordKey("x", "one") == recordKey("x", "two") {
		t.Error("same id + changed text must yield different keys")
	}
	// Same id, same text → same key (idempotent).
	if recordKey("x", "one") != recordKey("x", "one") {
		t.Error("same id + same text must be stable")
	}
	// No id → content-hash fallback (distinct prefix from id-keyed).
	if !strings.HasPrefix(recordKey("", "one"), "h:") {
		t.Error("empty id must fall back to content-hash key")
	}
}

func TestGroupSessionsGroupingAndDefaults(t *testing.T) {
	now := time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC)
	recs := []ingestRecord{
		{Text: "x", ID: "a", Session: "feed"}, // grouped
		{Text: "y", ID: "b", Session: "feed"}, // grouped (same session)
		{Text: "z", ID: "c"},                  // own session, id-derived
		{Text: "w"},                           // own session, hash-derived; ts defaulted
	}
	sess := groupSessions(recs, now)
	// feed has 2 records; the other two are their own sessions → 3 sessions total.
	if len(sess) != 3 {
		t.Fatalf("got %d sessions, want 3", len(sess))
	}
	for _, s := range sess {
		if !strings.HasPrefix(s.Session, "file:") {
			t.Errorf("session %q missing file: prefix", s.Session)
		}
		for i, r := range s.Records {
			if r.TS == "" {
				t.Error("RawRecord.TS must never be empty")
			}
			if r.Role != "document" {
				t.Errorf("default role = %q, want document", r.Role)
			}
			if r.Seq != i {
				t.Errorf("seq = %d, want %d (per-session ordinal)", r.Seq, i)
			}
			if r.Session != s.Session {
				t.Error("RawRecord.Session must equal its group session")
			}
		}
		if len(s.Keys) != len(s.Records) {
			t.Error("Keys must parallel Records")
		}
	}
}

func TestCmdIngestWritesL0AndDedups(t *testing.T) {
	home := t.TempDir()
	t.Setenv("WITNESS_HOME", home)
	t.Setenv("WITNESS_PROMPTS", filepath.Join("..", "..", "prompts"))
	nd := `{"text":"alpha","id":"a","session":"feed","ts":"2026-07-01T00:00:00Z"}
{"text":"beta","id":"b","session":"feed","ts":"2026-07-02T00:00:00Z"}
`
	ing, skip, err := cmdIngest(strings.NewReader(nd), true)
	if err != nil {
		t.Fatalf("cmdIngest: %v", err)
	}
	if ing != 2 || skip != 0 {
		t.Fatalf("ingested=%d skipped=%d, want 2/0", ing, skip)
	}
	st, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if n := st.RawCount("file:feed"); n != 2 {
		t.Fatalf("raw count for file:feed = %d, want 2", n)
	}
	// Re-ingest identical → idempotent skip (no growth).
	ing2, _, _ := cmdIngest(strings.NewReader(nd), true)
	if ing2 != 0 {
		t.Fatalf("re-ingest identical wrote %d records, want 0 (idempotent)", ing2)
	}
}

// TestIncrementalAppend verifies that ingesting new records under an existing shared
// session APPENDS (never wipes). This is the CRITICAL fix: before, the second ingest
// would replace=true and DELETE a1/a2, leaving only a3.
func TestIncrementalAppend(t *testing.T) {
	home := t.TempDir()
	t.Setenv("WITNESS_HOME", home)
	t.Setenv("WITNESS_PROMPTS", filepath.Join("..", "..", "prompts"))

	// Ingest two records under session "AAPL".
	batch1 := `{"text":"record one","id":"a1","session":"AAPL"}
{"text":"record two","id":"a2","session":"AAPL"}
`
	ing1, _, err := cmdIngest(strings.NewReader(batch1), true)
	if err != nil {
		t.Fatalf("batch1: %v", err)
	}
	if ing1 != 2 {
		t.Fatalf("batch1: ingested %d, want 2", ing1)
	}

	st, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	if n := st.RawCount("file:AAPL"); n != 2 {
		t.Fatalf("after batch1: raw count = %d, want 2", n)
	}
	recs1, _ := st.ReadRaw("file:AAPL")
	if len(recs1) != 2 || recs1[0].Text != "record one" || recs1[1].Text != "record two" {
		t.Fatalf("after batch1: unexpected records: %+v", recs1)
	}

	// Ingest a third record (new id only) under the SAME session.
	batch2 := `{"text":"record three","id":"a3","session":"AAPL"}
`
	ing2, _, err := cmdIngest(strings.NewReader(batch2), true)
	if err != nil {
		t.Fatalf("batch2: %v", err)
	}
	if ing2 != 1 {
		t.Fatalf("batch2: ingested %d, want 1", ing2)
	}

	// CRITICAL: count must be 3 (a1, a2, a3 all present), NOT 1 (which would mean a1/a2 were wiped).
	if n := st.RawCount("file:AAPL"); n != 3 {
		t.Fatalf("after batch2: raw count = %d, want 3 (a1/a2 must NOT be deleted)", n)
	}

	recs2, _ := st.ReadRaw("file:AAPL")
	if len(recs2) != 3 {
		t.Fatalf("after batch2: record count = %d, want 3", len(recs2))
	}
	// Verify all three texts are present (order: a1, a2, a3).
	texts := []string{recs2[0].Text, recs2[1].Text, recs2[2].Text}
	want := []string{"record one", "record two", "record three"}
	for i, got := range texts {
		if got != want[i] {
			t.Errorf("record[%d].Text = %q, want %q", i, got, want[i])
		}
	}
}

// TestSameIDUpdate verifies that re-ingesting the same id with CHANGED text updates
// the record in place (count stays the same, text changes).
func TestSameIDUpdate(t *testing.T) {
	home := t.TempDir()
	t.Setenv("WITNESS_HOME", home)
	t.Setenv("WITNESS_PROMPTS", filepath.Join("..", "..", "prompts"))

	// Ingest one record.
	batch1 := `{"text":"one","id":"a1","session":"S"}
`
	ing1, _, err := cmdIngest(strings.NewReader(batch1), true)
	if err != nil {
		t.Fatalf("batch1: %v", err)
	}
	if ing1 != 1 {
		t.Fatalf("batch1: ingested %d, want 1", ing1)
	}

	st, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	if n := st.RawCount("file:S"); n != 1 {
		t.Fatalf("after batch1: raw count = %d, want 1", n)
	}
	recs1, _ := st.ReadRaw("file:S")
	if recs1[0].Text != "one" {
		t.Fatalf("after batch1: text = %q, want \"one\"", recs1[0].Text)
	}

	// Re-ingest the SAME id with CHANGED text.
	batch2 := `{"text":"two","id":"a1","session":"S"}
`
	ing2, _, err := cmdIngest(strings.NewReader(batch2), true)
	if err != nil {
		t.Fatalf("batch2: %v", err)
	}
	// We expect 1 update (not 0, not 2 — the record is updated in place).
	if ing2 != 1 {
		t.Fatalf("batch2: ingested %d, want 1 (update)", ing2)
	}

	// Count must STAY 1 (not grow to 2).
	if n := st.RawCount("file:S"); n != 1 {
		t.Fatalf("after batch2: raw count = %d, want 1 (update in place)", n)
	}

	recs2, _ := st.ReadRaw("file:S")
	if len(recs2) != 1 {
		t.Fatalf("after batch2: record count = %d, want 1", len(recs2))
	}
	// Text must now be "two".
	if recs2[0].Text != "two" {
		t.Errorf("after batch2: text = %q, want \"two\"", recs2[0].Text)
	}
}

// TestIdempotentReIngest verifies that re-ingesting an identical batch (same ids,
// same text) writes 0 new records (idempotent, no change).
func TestIdempotentReIngest(t *testing.T) {
	home := t.TempDir()
	t.Setenv("WITNESS_HOME", home)
	t.Setenv("WITNESS_PROMPTS", filepath.Join("..", "..", "prompts"))

	batch := `{"text":"alpha","id":"a","session":"feed"}
{"text":"beta","id":"b","session":"feed"}
`
	// First ingest.
	ing1, _, err := cmdIngest(strings.NewReader(batch), true)
	if err != nil {
		t.Fatalf("first ingest: %v", err)
	}
	if ing1 != 2 {
		t.Fatalf("first ingest: wrote %d, want 2", ing1)
	}

	st, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	// Re-ingest identical → 0 new records.
	ing2, _, err := cmdIngest(strings.NewReader(batch), true)
	if err != nil {
		t.Fatalf("second ingest: %v", err)
	}
	if ing2 != 0 {
		t.Fatalf("second ingest: wrote %d, want 0 (idempotent)", ing2)
	}

	// Count stays 2.
	if n := st.RawCount("file:feed"); n != 2 {
		t.Fatalf("after re-ingest: count = %d, want 2", n)
	}
}

// TestOneRecordPerSession verifies that a record with no session field gets its own
// session (id-derived, or hash-derived if no id), and still works correctly.
func TestOneRecordPerSession(t *testing.T) {
	home := t.TempDir()
	t.Setenv("WITNESS_HOME", home)
	t.Setenv("WITNESS_PROMPTS", filepath.Join("..", "..", "prompts"))

	// Two records, each its own session (no session field).
	batch := `{"text":"standalone one","id":"x"}
{"text":"standalone two","id":"y"}
`
	ing, _, err := cmdIngest(strings.NewReader(batch), true)
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if ing != 2 {
		t.Fatalf("ingested %d, want 2", ing)
	}

	st, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	// Each record should be in its own session: file:x and file:y.
	if n := st.RawCount("file:x"); n != 1 {
		t.Fatalf("file:x count = %d, want 1", n)
	}
	if n := st.RawCount("file:y"); n != 1 {
		t.Fatalf("file:y count = %d, want 1", n)
	}
}

// idFromRecordKey must be the exact inverse of recordKey. recordKey APPENDS the hash
// ("<id>:<hash>"), so the id must be recovered by splitting at the LAST colon — the id
// itself may contain colons. Splitting at the FIRST colon truncated any such id (a URL
// -> "https", "arxiv:2301.12345" -> "arxiv", an ISO timestamp -> "2026-08-04T12"), so the
// dedup lookup missed and the record was re-appended on EVERY ingest.
func TestIDFromRecordKeyRoundTripsColonBearingIDs(t *testing.T) {
	for _, id := range []string{
		"post-1",
		"https://example.com/post-1",
		"arxiv:2301.12345",
		"2026-08-04T12:00:00Z",
		"urn:isbn:0451450523",
		"h", // a literal "h" id: key is "h:<8-byte hash>", still an id-keyed record
	} {
		key := recordKey(id, "some text")
		got, ok := idFromRecordKey(key)
		if id == "h" {
			// Ambiguous with the hash-only namespace by construction; documented as such.
			continue
		}
		if !ok || got != id {
			t.Errorf("recordKey/idFromRecordKey round trip broken for %q: got %q ok=%v (key %q)", id, got, ok, key)
		}
	}
	// The hash-only fallback has no stable id to merge on.
	if _, ok := idFromRecordKey(recordKey("", "body")); ok {
		t.Error(`the "h:<hash>" fallback must report ok=false (no stable id)`)
	}
	// Malformed keys.
	for _, bad := range []string{"", "nocolon", ":leading"} {
		if _, ok := idFromRecordKey(bad); ok {
			t.Errorf("malformed key %q must report ok=false", bad)
		}
	}
}

// End to end: re-ingesting the SAME record with a colon-bearing id must be idempotent.
// Before the fix each pass re-appended it — unbounded L0 growth for exactly the id shapes
// a document/paper feed uses (URLs, arXiv ids, timestamps).
func TestIngestIsIdempotentForColonBearingIDs(t *testing.T) {
	home := t.TempDir()
	t.Setenv("WITNESS_HOME", home)
	t.Setenv("WITNESS_PROMPTS", filepath.Join("..", "..", "prompts"))

	batch := `{"text":"a paper abstract","id":"arxiv:2301.12345","session":"papers"}
{"text":"a blog post","id":"https://example.com/post-1","session":"papers"}
`
	for pass := 1; pass <= 3; pass++ {
		if _, _, err := cmdIngest(strings.NewReader(batch), true); err != nil {
			t.Fatalf("pass %d: %v", pass, err)
		}
		st, err := store.Open()
		if err != nil {
			t.Fatal(err)
		}
		n := st.RawCount("file:papers")
		st.Close()
		if n != 2 {
			t.Fatalf("pass %d: raw count = %d, want 2 (re-ingest must dedup, not re-append)", pass, n)
		}
	}

	// An EDITED body under the same colon-bearing id must UPDATE, not duplicate.
	edited := `{"text":"a paper abstract, revised","id":"arxiv:2301.12345","session":"papers"}
`
	if _, _, err := cmdIngest(strings.NewReader(edited), true); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if n := st.RawCount("file:papers"); n != 2 {
		t.Fatalf("after an edit: raw count = %d, want 2 (update in place)", n)
	}
	recs, err := st.ReadRaw("file:papers")
	if err != nil {
		t.Fatal(err)
	}
	var sawRevised bool
	for _, r := range recs {
		if strings.Contains(r.Text, "revised") {
			sawRevised = true
		}
	}
	if !sawRevised {
		t.Fatalf("the edited body must be stored, got %+v", recs)
	}
}
