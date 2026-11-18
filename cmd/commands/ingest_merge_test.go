package commands

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/IngTian/witness/internal/store"
)

// A merge appends another machine's copy of a feed onto ours (rule 4) without importer keys for
// the rows it adds: the copy arrives under a minted id, and nothing ever imported into that id.
// Those rows are the merge's, not a desync, so `witness ingest` must neither reset the feed's
// progress over them nor re-mine what it already mined.

// mergeCopyOf merges, into the archive at WITNESS_HOME, an archive holding session with one
// document row per text.
func mergeCopyOf(t *testing.T, session string, texts ...string) *store.MergeReport {
	t.Helper()
	home := t.TempDir()
	ours := os.Getenv("WITNESS_HOME")
	t.Setenv("WITNESS_HOME", home)
	inc, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	for i, text := range texts {
		if err := inc.AppendRaw(store.RawRecord{TS: "2026-03-01T00:00:00Z", Session: session, Seq: i, Role: "document", Text: text}); err != nil {
			t.Fatal(err)
		}
	}
	path := filepath.Join(t.TempDir(), "incoming.db")
	if err := inc.Export(path, false); err != nil {
		t.Fatal(err)
	}
	inc.Close()
	t.Setenv("WITNESS_HOME", ours)
	st := openBase(t)
	rep, err := st.Merge(context.Background(), path, true, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return rep
}

func ingestOne(t *testing.T, id, text string) {
	t.Helper()
	if _, _, err := cmdIngest(strings.NewReader(`{"text":"`+text+`","id":"`+id+`","session":"feed"}`+"\n"), true); err != nil {
		t.Fatal(err)
	}
}

// feedState is file:feed's row texts, importer-key count and default-lens watermark.
func feedState(t *testing.T) (texts []string, keys, progress int) {
	t.Helper()
	for _, r := range rawRows(t, "file:feed") {
		texts = append(texts, r.Text)
	}
	st := openBase(t)
	return texts, len(parseImportKeysJSON(st.MetaString(store.FileImportKeysNS + ":file:feed"))), st.DistilledCount("file:feed", store.LensDefault)
}

func markMined(t *testing.T, n int) {
	t.Helper()
	if err := openBase(t).MarkDistilled("file:feed", store.LensDefault, n); err != nil {
		t.Fatal(err)
	}
}

// One new record after a merge-append: progress stays, and the one new turn is all that is pending.
// Before, the key list stayed one row short for good, so EVERY later ingest took the replace path
// and reset the feed's progress: the whole feed was mined again each time.
func TestIngestAfterAMergeAppendKeepsProgress(t *testing.T) {
	ingestHome(t)
	ingestOne(t, "a", "body a")
	markMined(t, 1)
	if rep := mergeCopyOf(t, "file:feed@00000000", "body a", "body c1"); rep.Sessions.Appended != 1 {
		t.Fatalf("setup: want c1 appended onto our feed, got %+v", rep.Sessions)
	}
	markMined(t, 2) // the worker mines c1

	for i, rec := range []struct{ id, text string }{{"d", "body d"}, {"e", "body e"}} {
		ingestOne(t, rec.id, rec.text)
		texts, keys, progress := feedState(t)
		if progress != 2 || len(texts) != 3+i || keys != len(texts) {
			t.Fatalf("after ingest %d: progress %d (want 2), rows %v, %d keys: the mined turns are mined again", i+1, progress, texts, keys)
		}
	}
}

// An edit of an earlier record still replaces the feed and resets its progress, as before, and
// keeps the merged row; the ingest after it keeps progress.
func TestIngestEditAfterAMergeAppendStillReplaces(t *testing.T) {
	ingestHome(t)
	ingestOne(t, "a", "body a")
	markMined(t, 1)
	mergeCopyOf(t, "file:feed@00000000", "body a", "body c1")
	markMined(t, 2)

	ingestOne(t, "a", "body a EDITED")
	texts, keys, progress := feedState(t)
	if progress != 0 || strings.Join(texts, "|") != "body a EDITED|body c1" || keys != 2 {
		t.Fatalf("after the edit: progress %d, rows %v, %d keys; want 0, [body a EDITED, body c1], 2", progress, texts, keys)
	}
	markMined(t, 2)
	ingestOne(t, "d", "body d")
	if texts, keys, progress := feedState(t); progress != 2 || len(texts) != 3 || keys != 3 {
		t.Fatalf("after the next ingest: progress %d (want 2), rows %v, %d keys", progress, texts, keys)
	}
}
