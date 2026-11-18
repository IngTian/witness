package commands

import (
	"context"
	"fmt"
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

// ingestFeed ingests one batch of records into "feed", given as id, text pairs ("" for no id).
func ingestFeed(t *testing.T, idText ...string) {
	t.Helper()
	var b strings.Builder
	for i := 0; i < len(idText); i += 2 {
		fmt.Fprintf(&b, `{"text":%q,"id":%q,"session":"feed"}`+"\n", idText[i+1], idText[i])
	}
	if _, _, err := cmdIngest(strings.NewReader(b.String()), true); err != nil {
		t.Fatal(err)
	}
}

// mergeArchive merges the archive at from into the archive at into, and leaves WITNESS_HOME at into.
func mergeArchive(t *testing.T, into, from string) *store.MergeReport {
	t.Helper()
	path := filepath.Join(t.TempDir(), "incoming.db")
	t.Setenv("WITNESS_HOME", from)
	if err := openBase(t).Export(path, false); err != nil {
		t.Fatal(err)
	}
	t.Setenv("WITNESS_HOME", into)
	rep, err := openBase(t).Merge(context.Background(), path, true, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return rep
}

// Three machines ingest one feed. Y first had an older record 1, so it holds Z's feed as a copy, and
// relays Z's record 2 to X, whose own feed the merge grows by that row (rule 4), keyed by content
// only. X's next ingest of the feed sends record 2 under its id: it takes over that row, and only a
// new record is added. Keyed anew, record 2 was held twice and its second copy mined again.
func TestReingestOfAFeedAMergeGrewHoldsEachRecordOnce(t *testing.T) {
	for _, c := range []struct {
		name  string
		batch []string
		want  string
	}{
		{"the feed as it is", []string{"1", "body a", "2", "body c1"}, "body a|body c1"},
		{"the feed and a new record", []string{"1", "body a", "2", "body c1", "3", "body d"}, "body a|body c1|body d"},
	} {
		t.Run(c.name, func(t *testing.T) {
			ingestHome(t)
			x, y, z := t.TempDir(), t.TempDir(), t.TempDir()
			t.Setenv("WITNESS_HOME", z)
			ingestFeed(t, "1", "body a")
			t.Setenv("WITNESS_HOME", y)
			ingestFeed(t, "1", "body y-old")
			mergeArchive(t, y, z)
			t.Setenv("WITNESS_HOME", z)
			ingestFeed(t, "2", "body c1")
			mergeArchive(t, y, z)
			t.Setenv("WITNESS_HOME", x)
			ingestFeed(t, "1", "body a")
			markMined(t, 1)
			if rep := mergeArchive(t, x, y); rep.Sessions.Appended != 1 {
				t.Fatalf("setup: want Y's copy of Z's feed appended onto X's, got %+v", rep.Sessions)
			}
			markMined(t, 2)

			ingestFeed(t, c.batch...)
			if texts, keys, progress := feedState(t); strings.Join(texts, "|") != c.want || keys != len(texts) || progress != 2 {
				t.Errorf("X's feed is %v, %d keys, progress %d; want [%s], a key a row, progress 2", texts, keys, progress, c.want)
			}
		})
	}
}

// The feed holds one row keyed by its text only, and the batch two records of that text, one with an
// id. An id-less record and an id'd one are two records: the id-less one keeps its row, and the id'd
// one adds its own (taken over, the row was gone for the id-less one, which the next ingest appended
// again). Two id'd records take the row over once: the second adds its own (both took it, and the
// first was lost).
func TestIngestTakesOverOnlyAnUnclaimedRowOfItsText(t *testing.T) {
	for name, batch := range map[string][]string{
		"and an id-less one":   {"1", "body x", "", "body x"},
		"and another id'd one": {"1", "body x", "2", "body x"},
	} {
		t.Run(name, func(t *testing.T) {
			ingestHome(t)
			ingestFeed(t, "", "body x")
			for i := range 2 {
				ingestFeed(t, batch...)
				if texts, keys, _ := feedState(t); len(texts) != 2 || keys != 2 {
					t.Fatalf("after ingest %d: rows %v, %d keys; want each record held once", i+1, texts, keys)
				}
			}
		})
	}
}
