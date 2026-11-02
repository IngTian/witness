package opencode

import (
	"context"
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
	mutateOpenCodeDB(t, dbPath, `INSERT INTO session VALUES ('ses_empty', '/repo', 'nothing said yet', 1000, 6000),
		('ses_scratch', '/repo', 'witness-distill', 1000, 7000)`)
	t.Setenv("WITNESS_HOME", filepath.Join(t.TempDir(), "witness"))
	st, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	var got [][3]any
	ctx := platform.WithImportProgress(context.Background(), func(done, total int, open bool) {
		got = append(got, [3]any{done, total, open})
	})
	stats, err := (&Importer{Store: st, DBPath: dbPath}).Import(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Sessions != 1 || stats.Records != 2 {
		t.Fatalf("import itself changed: %+v", stats)
	}
	want := [][3]any{{0, 2, false}, {1, 2, false}, {2, 2, false}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("progress = %v, want %v", got, want)
	}
}
