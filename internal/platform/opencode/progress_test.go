package opencode

import (
	"context"
	"database/sql"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/IngTian/witness/internal/platform"
	"github.com/IngTian/witness/internal/store"
)

// The OpenCode session list is read before the first import, so `witness import --agent opencode`
// reports an exact total from its first line, one step per session read — a session with no text
// counts too (it was read), and witness's own legacy scratch session is not work at all.
func TestImportProgressCountsEverySessionRead(t *testing.T) {
	dbPath := seedOpenCodeDB(t)
	// v2 rows: ses_empty (no messages, updated 6000), ses_scratch (agent=witness-distill, updated 7000)
	mutateV2DB := func(path, stmt string) {
		db, err := sql.Open("sqlite", path)
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		if _, err := db.Exec(stmt); err != nil {
			t.Fatal(err)
		}
	}
	mutateV2DB(dbPath, `
		INSERT INTO session_v2 VALUES ('ses_empty', 'proj1', NULL, NULL, NULL, NULL, 'empty', '/repo', NULL, NULL, '2.2.7', NULL, NULL, NULL, NULL, NULL, NULL, 0, 0, 0, 0, 0, 0, NULL, NULL, NULL, NULL, 1000, 6000, NULL, NULL, NULL, NULL, NULL, NULL, NULL, 0);
		INSERT INTO session_v2 VALUES ('ses_scratch', 'proj1', NULL, NULL, NULL, NULL, 'scratch', '/repo', NULL, NULL, '2.2.7', NULL, NULL, NULL, NULL, NULL, NULL, 0, 0, 0, 0, 0, 0, NULL, NULL, 'witness-distill', NULL, 1000, 7000, NULL, NULL, NULL, NULL, NULL, NULL, NULL, 0);
	`)
	t.Setenv("WITNESS_HOME", filepath.Join(t.TempDir(), "witness"))
	st, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	var got [][2]int
	ctx := platform.WithImportProgress(context.Background(), func(done, total int) {
		got = append(got, [2]int{done, total})
	})
	stats, err := (&Importer{Store: st, DBPath: dbPath}).Import(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Sessions != 1 || stats.Records != 2 {
		t.Fatalf("import itself changed: %+v", stats)
	}
	want := [][2]int{{0, 2}, {1, 2}, {2, 2}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("progress = %v, want %v", got, want)
	}
}
