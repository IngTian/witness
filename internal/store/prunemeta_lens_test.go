package store

import "testing"

// Pruning a session must not delete PER-LENS meta rows.
//
// PruneSessionsBefore reaps orphaned per-SESSION meta ("<namespace>:<session>", today
// opencode_import_keys / file_import_keys) by matching everything after the first colon against
// the session id. That predicate is namespace-BLIND: three live namespaces share the same
// `<ns>:<x>` shape but key on a LENS, not a session — review_rowid:<lens> (config.go),
// profile_sig:<lens> (distill/summarize.go) and emerge_seen:<lens>:<sig> (distill/emergent.go).
// So a session whose id happens to equal a lens name collides with all three.
//
// Why such an id is reachable: `witness capture` takes the session id from the hook payload and
// `witness ingest` lets the caller supply one; nothing constrains either to a UUID. The damage is
// invisible and large — deleting review_rowid:<lens> resets that lens's review watermark to zero,
// so the next review re-folds the ENTIRE L1 history into L2 (duplicate facet assertions across
// the whole archive); losing profile_sig forces a full profile regeneration; losing emerge_seen
// re-proposes every long arc the user already dismissed.
func TestPruneSessionsBeforeKeepsPerLensMetaRows(t *testing.T) {
	s := tempStore(t)

	// A stale session named exactly like a lens.
	const lens = "default"
	if err := s.AppendRaw(RawRecord{
		TS: "2020-01-01T00:00:00Z", Session: lens, Role: "user", Text: "an old turn",
	}); err != nil {
		t.Fatal(err)
	}

	// The three per-LENS keys, which must SURVIVE.
	perLens := map[string]string{
		"review_rowid:" + lens:          "579",
		"profile_sig:" + lens:           "abc123",
		"emerge_seen:" + lens + ":sig1": "1",
	}
	for k, v := range perLens {
		if err := s.SetMetaString(k, v); err != nil {
			t.Fatal(err)
		}
	}
	// And a genuine per-SESSION key, which must still be REAPED.
	if err := s.SetMetaString("file_import_keys:"+lens, `["a:1"]`); err != nil {
		t.Fatal(err)
	}

	if _, _, err := s.PruneSessionsBefore("2025-01-01T00:00:00Z"); err != nil {
		t.Fatal(err)
	}

	for k, want := range perLens {
		if got := s.MetaString(k); got != want {
			t.Errorf("pruning a session named %q deleted the per-LENS meta key %q (was %q, now %q) — "+
				"a reset review watermark re-folds the whole L1 history into L2", lens, k, want, got)
		}
	}
	// The reap must still work: the fix must not become "never clean up".
	if got := s.MetaString("file_import_keys:" + lens); got != "" {
		t.Errorf("the orphaned per-session meta row survived (%q) — the reap became a no-op", got)
	}
}

// Every namespace in the allowlist must actually be reaped, and the list must not silently
// shrink. An allowlist fails safe but it also fails SILENTLY: a per-session namespace nobody adds
// here just leaks rows forever, with no symptom until someone counts them.
func TestPerSessionMetaNamespacesAreAllReaped(t *testing.T) {
	if len(PerSessionMetaNamespaces) == 0 {
		t.Fatal("the allowlist is empty — PruneSessionsBefore now reclaims no meta at all")
	}
	for _, ns := range PerSessionMetaNamespaces {
		t.Run(ns, func(t *testing.T) {
			s := tempStore(t)
			const sess = "s-old"
			if err := s.AppendRaw(RawRecord{
				TS: "2020-01-01T00:00:00Z", Session: sess, Role: "user", Text: "old",
			}); err != nil {
				t.Fatal(err)
			}
			if err := s.SetMetaString(ns+":"+sess, "state"); err != nil {
				t.Fatal(err)
			}
			if _, _, err := s.PruneSessionsBefore("2025-01-01T00:00:00Z"); err != nil {
				t.Fatal(err)
			}
			if got := s.MetaString(ns + ":" + sess); got != "" {
				t.Errorf("%q:<session> was not reclaimed (%q) — dead meta rows leak for every "+
					"pruned session", ns, got)
			}
		})
	}
	// The two namespaces that exist today, pinned by name so a rename has to be deliberate.
	// Their definitions live outside this package (internal/platform/opencode/import.go,
	// cmd/commands/ingest.go), which the store cannot import, so this is the sync check.
	want := map[string]bool{"opencode_import_keys": true, "file_import_keys": true}
	for _, ns := range PerSessionMetaNamespaces {
		delete(want, ns)
	}
	for missing := range want {
		t.Errorf("%q dropped out of PerSessionMetaNamespaces — if the producing constant was "+
			"renamed, update both together or its rows leak forever", missing)
	}
}
