package codex

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"github.com/IngTian/witness/internal/platform"
)

type report struct {
	done, total int
	open        bool
}

func recordProgress(t *testing.T) (context.Context, *[]report) {
	t.Helper()
	var got []report
	return platform.WithImportProgress(context.Background(), func(done, total int, open bool) {
		got = append(got, report{done, total, open})
	}), &got
}

// The bar for `witness import --agent codex` must be TRUE: rollouts are all known before the first
// import, a history database's threads join the total once (when it is opened, the only time it
// is Open), and every input — including the two that fail here — ends counted, so it finishes at
// exactly its total instead of stalling short.
func TestImportProgressCountsEveryInputOnce(t *testing.T) {
	im, st := testImporter(t)
	db, err := sql.Open("sqlite", filepath.Join(im.Home, "thread_history_1.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, q := range []string{
		`CREATE TABLE thread_history_projection_state(thread_id TEXT PRIMARY KEY,next_rollout_ordinal INTEGER)`,
		`CREATE TABLE thread_items(thread_id TEXT,turn_id TEXT,item_id TEXT,rollout_ordinal INTEGER,created_at_ms INTEGER,item_json TEXT,item_type TEXT,PRIMARY KEY(thread_id,turn_id,item_id))`,
		// t2 cannot be decoded, which abandons the database before t3 is ever reached.
		`INSERT INTO thread_history_projection_state VALUES('t1',1),('t2',1),('t3',1)`,
		`INSERT INTO thread_items VALUES('t1','turn','u1',1,1793188800123,'{"type":"agentMessage","text":"hi"}','agentMessage')`,
		`INSERT INTO thread_items VALUES('t2','turn','u1',1,1793188800123,'not json','agentMessage')`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	// A history file this witness does not support: no threads, but it must stop being Open.
	if err := os.WriteFile(filepath.Join(im.Home, "thread_history_2.sqlite"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	rollout(t, im.Home, "r1", message("user", "one"))
	rollout(t, im.Home, "r2", message("user", "two"))
	bad := filepath.Join(im.Home, "archived_sessions", "broken.jsonl")
	if err := os.MkdirAll(filepath.Dir(bad), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bad, []byte("not a header\n"), 0600); err != nil {
		t.Fatal(err)
	}

	ctx, got := recordProgress(t)
	stats, err := im.Import(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Failed != 3 || st.RawCount("codex:t1") != 1 || st.RawCount("codex:r2") != 1 {
		t.Fatalf("import itself changed: %+v", stats)
	}

	const want = 3 + 3 // three rollouts, three history threads
	r := *got
	if len(r) == 0 || r[0] != (report{0, 3, true}) {
		t.Fatalf("first report must be the 3 listed rollouts, Open until the databases are read: %+v", r)
	}
	if last := r[len(r)-1]; last != (report{want, want, false}) {
		t.Fatalf("must finish at exactly its total, failures included: last %+v of %+v", last, r)
	}
	for i := 1; i < len(r); i++ {
		p, c := r[i-1], r[i]
		switch {
		case c.done < p.done || c.done > c.total:
			t.Fatalf("done went backwards or past total at %d: %+v", i, r)
		case !p.open && c.total != p.total:
			t.Fatalf("a total that is not Open moved at %d: %+v", i, r)
		case !p.open && c.open:
			t.Fatalf("reopened at %d: %+v", i, r)
		}
	}
}

// With one history database — the usual Codex home — the total is final as soon as it is opened,
// so nothing after that may be Open (that would print a floor, "≥", for an exact estimate).
func TestImportProgressIsClosedOnceTheOnlyDatabaseIsCounted(t *testing.T) {
	im, _ := testImporter(t)
	db := testHistoryDB(t, im.Home)
	sqlMessages(t, db)
	if _, err := db.Exec(`INSERT INTO thread_history_projection_state VALUES('s2',1)`); err != nil {
		t.Fatal(err)
	}
	ctx, got := recordProgress(t)
	if _, err := im.Import(ctx, nil); err != nil {
		t.Fatal(err)
	}
	r := *got
	if r[0] != (report{0, 0, true}) || r[len(r)-1] != (report{2, 2, false}) {
		t.Fatalf("%+v", r)
	}
	for _, x := range r[1:] {
		if x.open || x.total != 2 {
			t.Fatalf("once counted, the total is exact: %+v", r)
		}
	}
}

// Without a reporter in the context — the capture hook, every existing caller — nothing changes.
func TestImportWithoutProgressIsUnchanged(t *testing.T) {
	im, _ := testImporter(t)
	rollout(t, im.Home, "s1", message("user", "hello"))
	if s := runImport(t, im); s.Records != 1 || s.Sessions != 1 {
		t.Fatalf("%+v", s)
	}
}
