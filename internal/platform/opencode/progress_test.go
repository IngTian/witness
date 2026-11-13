package opencode

import (
	"context"
	"reflect"
	"testing"

	"github.com/IngTian/witness/internal/platform"
)

// The OpenCode session list is read before the first import, so `witness import --agent opencode`
// reports an exact total from its first line, one step per session read — a session with no text
// counts too (it was read), and witness's own legacy scratch session is not work at all.
func TestImportProgressCountsEverySessionRead(t *testing.T) {
	dbPath := seedOpenCodeDB(t)
	mutateV2DB(t, dbPath,
		session("ses_empty", 6000), // no messages
		session("ses_scratch", 7000, "agent", "witness-distill"),
	)
	st := testStore(t)

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
