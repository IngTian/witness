package platform

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/IngTian/witness/internal/store"
)

// The chunk budget is counted in CHARACTERS, not bytes.
//
// That is what ChunkPolicy.MaxChars promises and what the drain's own sizer measures
// (store.PendingInputChars uses SQLite LENGTH(), which counts characters). RenderChunks used
// Go len(), making the effective budget ~3x tighter for CJK text: measured at 10 records of
// 400 Chinese characters with a budget above the whole corpus's 4080 characters, it emitted
// 8 chunks where the same character count in ASCII emitted 1. Chunking measurably degrades
// arc lenses — whole-session is the default for that reason — so a part-Chinese archive
// silently got the worst-quality path on content that fit.
func TestRenderChunksBudgetsInCharactersNotBytes(t *testing.T) {
	const perRecord = 400
	build := func(ch string) []store.RawRecord {
		body := strings.Repeat(ch, perRecord)
		out := make([]store.RawRecord, 0, 10)
		for i := 0; i < 10; i++ {
			out = append(out, store.RawRecord{Role: "user", Text: body})
		}
		return out
	}
	charSize := func(raw []store.RawRecord) int {
		n := 0
		for _, r := range raw {
			n += utf8.RuneCountInString(r.Text) + utf8.RuneCountInString(r.Role) + 4
		}
		return n
	}

	cjk, ascii := build("观"), build("a")
	if charSize(cjk) != charSize(ascii) {
		t.Fatalf("fixture is wrong: %d vs %d characters", charSize(cjk), charSize(ascii))
	}
	budget := charSize(cjk) + 100 // comfortably fits the whole corpus, by characters

	got := RenderChunks(cjk, ChunkPolicy{MaxChars: budget})
	if len(got) != 1 {
		t.Errorf("CJK corpus of %d characters under a %d-character budget split into %d chunks; "+
			"the budget is being measured in bytes", charSize(cjk), budget, len(got))
	}
	if want := RenderChunks(ascii, ChunkPolicy{MaxChars: budget}); len(got) != len(want) {
		t.Errorf("same character count chunked differently by script: CJK %d vs ASCII %d",
			len(got), len(want))
	}
}

// Splitting still happens when the CHARACTER count genuinely exceeds the budget, and every
// record survives — the guard must not become "never chunk".
func TestRenderChunksStillSplitsOnCharacterOverflow(t *testing.T) {
	var raw []store.RawRecord
	for i := 0; i < 10; i++ {
		raw = append(raw, store.RawRecord{Role: "user", Text: strings.Repeat("观", 100)})
	}
	chunks := RenderChunks(raw, ChunkPolicy{MaxChars: 250}) // ~2 records per chunk
	if len(chunks) < 2 {
		t.Fatalf("a corpus over budget must split: got %d chunk(s)", len(chunks))
	}
	// Each chunk must respect the budget, in CHARACTERS.
	//
	// Asserted unconditionally. The earlier version guarded this with
	// `n > 250 && !strings.Contains(c, strings.Repeat("观", 100))`, meaning to allow the
	// lone-oversized-record exception — but every record here IS that 100-char string and
	// RenderTranscript copies Text verbatim, so EVERY chunk contains it and the exception
	// swallowed all of them. The loop body was unreachable and asserted nothing. The exception is
	// not needed anyway: no single record here exceeds the budget (108 chars each), so every
	// chunk must fit, and the dedicated lone-giant case is
	// TestRenderChunksEmitsAnOversizedMultibyteRecordAlone below.
	for i, c := range chunks {
		if n := utf8.RuneCountInString(c); n > 250 {
			t.Errorf("chunk %d is %d characters, over the 250-character budget", i, n)
		}
		// Byte length exceeding the budget is expected and fine for CJK — that is the whole
		// point. Assert it, so a silent regression to byte-budgeting is visible here too.
		if len(c) <= utf8.RuneCountInString(c) {
			t.Errorf("chunk %d is not multi-byte (%d bytes, %d chars); the fixture no longer "+
				"exercises the bytes-vs-characters distinction", i, len(c), utf8.RuneCountInString(c))
		}
	}
	// No record is dropped.
	joined := strings.Join(chunks, "")
	if want := strings.Count(joined, strings.Repeat("观", 100)); want < 10 {
		t.Errorf("records were lost: found %d of 10", want)
	}
}

// A single record larger than the whole budget is still emitted alone rather than dropped
// or looping forever — including when it is multi-byte.
func TestRenderChunksEmitsAnOversizedMultibyteRecordAlone(t *testing.T) {
	raw := []store.RawRecord{
		{Role: "user", Text: strings.Repeat("观", 500)},
		{Role: "assistant", Text: "short"},
	}
	chunks := RenderChunks(raw, ChunkPolicy{MaxChars: 50})
	if len(chunks) == 0 {
		t.Fatal("the oversized record was dropped")
	}
	if !strings.Contains(chunks[0], strings.Repeat("观", 500)) {
		t.Error("the oversized record is not in the first chunk")
	}
}
