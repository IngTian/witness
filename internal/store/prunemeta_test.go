package store

import "testing"

// Pruning one session must not delete a DIFFERENT, live session's per-session meta.
//
// PruneSessionsBefore cleans up "<namespace>:<session>" meta rows so they don't leak.
// It used to do that with a bare suffix match on ":"+session — but session ids may
// themselves contain colons (ingest builds "file:" + a caller-supplied id, which is an
// arbitrary string: a path, a URL, an arxiv id). So pruning "file:x" also matched the LIVE
// session "file:notes:file:x". The damage is invisible: that session's file_import_keys row
// is its ingest dedup state, so the next `witness ingest` sees a never-ingested session and
// re-appends every already-durable record.
func TestPruneSessionsBeforeKeepsAnotherLiveSessionsMeta(t *testing.T) {
	s := tempStore(t)
	const (
		stale = "file:x"
		live  = "file:notes:file:x" // legal id, and ends in ":" + stale
	)
	if err := s.AppendRaw(RawRecord{TS: "2020-01-01T00:00:00Z", Session: stale, Role: "document", Text: "old"}); err != nil {
		t.Fatal(err)
	}
	if err := s.AppendRaw(RawRecord{TS: "2030-01-01T00:00:00Z", Session: live, Role: "document", Text: "new"}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetMetaString("file_import_keys:"+stale, `["a:1"]`); err != nil {
		t.Fatal(err)
	}
	if err := s.SetMetaString("file_import_keys:"+live, `["b:2"]`); err != nil {
		t.Fatal(err)
	}

	sessions, _, err := s.PruneSessionsBefore("2025-01-01T00:00:00Z")
	if err != nil {
		t.Fatal(err)
	}
	if sessions != 1 {
		t.Fatalf("pruned %d sessions, want only the stale one", sessions)
	}

	// The live session is untouched: raw rows AND its dedup state.
	if n := s.RawCount(live); n != 1 {
		t.Fatalf("live session lost raw rows: %d", n)
	}
	if got := s.MetaString("file_import_keys:" + live); got != `["b:2"]` {
		t.Errorf("pruning %q deleted the LIVE session %q's ingest dedup state (got %q) — "+
			"its next ingest would re-append every already-stored record", stale, live, got)
	}
	// The stale session's own meta still gets reclaimed (no leak).
	if got := s.MetaString("file_import_keys:" + stale); got != "" {
		t.Errorf("the pruned session's meta leaked: %q", got)
	}
}

// The ordinary namespaced case must still be reclaimed, including an OpenCode session id
// (which contains both a colon and an underscore — the reason this is not a LIKE).
func TestPruneSessionsBeforeStillReclaimsNamespacedMeta(t *testing.T) {
	s := tempStore(t)
	sess := "opencode:ses_abc_123"
	if err := s.AppendRaw(RawRecord{TS: "2020-01-01T00:00:00Z", Session: sess, Role: "user", Text: "old"}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetMetaString("opencode_import_keys:"+sess, `["k"]`); err != nil {
		t.Fatal(err)
	}
	// Unrelated meta that must SURVIVE: a lens-keyed scalar and a plain scalar.
	if err := s.SetMetaString("review_rowid:default", "42"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetMetaString("worker_status", "idle"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.PruneSessionsBefore("2025-01-01T00:00:00Z"); err != nil {
		t.Fatal(err)
	}
	if got := s.MetaString("opencode_import_keys:" + sess); got != "" {
		t.Errorf("per-session meta was not reclaimed: %q", got)
	}
	if got := s.MetaString("review_rowid:default"); got != "42" {
		t.Errorf("clobbered the per-lens review watermark: %q", got)
	}
	if got := s.MetaString("worker_status"); got != "idle" {
		t.Errorf("clobbered an unrelated scalar: %q", got)
	}
}
