package commands

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/IngTian/witness/internal/store"
)

// applyIngestSession reads a session's stored key list AND its raw row count, then writes
// both back. Unserialized, a concurrent raw mutation (a second `witness ingest`, or
// `witness cleanup` reclaiming rows) desynchronizes oldKeys[i] from ReadRaw()[i], and the
// positional update then writes one record's text over a DIFFERENT record's row —
// corrupting an already-durable record. cmdIngest therefore holds ImportLock("file")
// across the whole read-modify-write, mirroring the OpenCode importer's use of the same
// primitive for the same hazard.
//
// This asserts the WIRING: with the lock already held (as a concurrent witness process
// would hold it), cmdIngest must DECLINE rather than proceed unserialized. Corrupting an
// already-durable record is worse than refusing a batch the user can simply re-run.
//
// Scope, stated honestly: the cross-PROCESS interleaving the lock exists to prevent needs
// two real witness processes to demonstrate, so it is not reproduced here. A same-process
// concurrency test would prove nothing either way — MaxOpenConns(1) already serializes
// those at the SQLite layer (verified: such a test passes with the lock removed, just far
// slower). The fix rests on the code shape plus that precedent, not on a red test.
func TestIngestDeclinesWhenTheImportLockIsHeld(t *testing.T) {
	home := t.TempDir()
	t.Setenv("WITNESS_HOME", home)
	t.Setenv("WITNESS_PROMPTS", filepath.Join("..", "..", "prompts"))

	// A first ingest, unobstructed, must succeed.
	first := `{"text":"body-A","id":"a","session":"shared"}` + "\n"
	n, _, err := cmdIngest(strings.NewReader(first), true)
	if err != nil {
		t.Fatalf("unobstructed ingest must succeed: %v", err)
	}
	if n != 1 {
		t.Fatalf("ingested %d, want 1", n)
	}

	// Now simulate a concurrent witness process holding the same lock.
	st, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	unlock, ok := st.ImportLock("file")
	if !ok {
		t.Fatal("precondition: the lock should be free here")
	}
	released := false
	release := func() {
		if !released {
			released = true
			unlock()
		}
	}
	defer release()

	second := `{"text":"body-B","id":"b","session":"shared"}` + "\n"
	if _, _, err := cmdIngest(strings.NewReader(second), true); err == nil {
		t.Fatal("ingest must DECLINE while another holds the import lock, not proceed unserialized")
	} else if !strings.Contains(err.Error(), "running") {
		t.Errorf("the error should tell the user to retry, got %q", err)
	}

	// The declined batch must not have landed.
	recs, err := st.ReadRaw("file:shared")
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 1 || !strings.Contains(recs[0].Text, "body-A") {
		t.Fatalf("a declined batch must write nothing; got %+v", recs)
	}

	// Once released, ingest works again.
	release()
	if _, _, err := cmdIngest(strings.NewReader(second), true); err != nil {
		t.Fatalf("ingest must succeed once the lock is released: %v", err)
	}
}
