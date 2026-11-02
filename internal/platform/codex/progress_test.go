package codex

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/IngTian/witness/internal/platform"
)

type report struct{ done, total int }

func recordProgress(t *testing.T) (context.Context, *[]report) {
	t.Helper()
	var got []report
	return platform.WithImportProgress(context.Background(), func(done, total int) {
		got = append(got, report{done, total})
	}), &got
}

// The bar for `witness import --agent codex` must be TRUE. Nothing is reported until the history
// database's threads are counted, so the first total is already the final one — a log keeps only
// that first line. Each thread ticks as it is reached, and every input, including the four that
// fail here, ends counted, so the bar finishes at exactly its total instead of stalling short.
func TestImportProgressCountsEveryInputOnce(t *testing.T) {
	im, st := testImporter(t)
	db, err := sql.Open("sqlite", filepath.Join(im.Home, historyDB))
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
	// History files this witness does not support, one sorting before the real database: neither
	// adds threads, and the one read first must not report a total the real database then grows.
	for _, name := range []string{"thread_history_0.sqlite", "thread_history_2.sqlite"} {
		if err := os.WriteFile(filepath.Join(im.Home, name), nil, 0600); err != nil {
			t.Fatal(err)
		}
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
	if stats.Failed != 4 || st.RawCount("codex:t1") != 1 || st.RawCount("codex:r1") != 1 || st.RawCount("codex:r2") != 1 {
		t.Fatalf("import itself changed: %+v", stats)
	}
	// 3 threads + 3 rollouts. t1 is reached (0), t2 is reached (1) and fails, the database is then
	// finished whole (3), the unsupported file after it changes nothing (3), then each rollout.
	want := []report{{0, 6}, {1, 6}, {3, 6}, {3, 6}, {4, 6}, {5, 6}, {6, 6}}
	if !reflect.DeepEqual(*got, want) {
		t.Fatalf("progress = %v, want %v", *got, want)
	}
}

// With one history database — the usual Codex home — every thread ticks as it is reached and the
// database's end counts the last one.
func TestImportProgressTicksEveryThread(t *testing.T) {
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
	if want := []report{{0, 2}, {1, 2}, {2, 2}}; !reflect.DeepEqual(*got, want) {
		t.Fatalf("progress = %v, want %v", *got, want)
	}
}

// A history database that fails before its threads are counted still stops holding the bar back:
// the rollouts are reported, and finish.
func TestImportProgressSurvivesAnUnreadableDatabase(t *testing.T) {
	im, _ := testImporter(t)
	if err := os.WriteFile(filepath.Join(im.Home, historyDB), nil, 0600); err != nil {
		t.Fatal(err) // an empty file: no projection table, so importDB fails before counting
	}
	rollout(t, im.Home, "r1", message("user", "one"))
	ctx, got := recordProgress(t)
	stats, err := im.Import(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if want := []report{{0, 1}, {1, 1}}; stats.Failed != 1 || !reflect.DeepEqual(*got, want) {
		t.Fatalf("progress = %v, want %v (failed %d)", *got, want, stats.Failed)
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
