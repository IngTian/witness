package file

import (
	"testing"

	"github.com/IngTian/witness/internal/platform"
	"github.com/IngTian/witness/internal/store"
)

func TestFilePlatformShape(t *testing.T) {
	p := Platform{}
	if p.Name() != "file" {
		t.Errorf("Name() = %q, want file", p.Name())
	}
	if p.SessionPrefix() != "file:" {
		t.Errorf("SessionPrefix() = %q, want file:", p.SessionPrefix())
	}
	// RenderInputs delegates to the shared shaper: one flat doc → one chunk.
	raw := []store.RawRecord{{Session: "file:x", Seq: 0, Role: "document", Text: "hello"}}
	got := p.RenderInputs(raw, platform.ChunkPolicy{})
	if len(got) != 1 {
		t.Fatalf("RenderInputs len = %d, want 1", len(got))
	}
}

// Ingested sessions are the corpus `witness view` leaves out of conversation activity; any other
// session is not.
func TestFileSessionsAreTheCorpus(t *testing.T) {
	if !platform.IsCorpusSession("file:/notes.md") {
		t.Error("a file: session must be a corpus session")
	}
	for _, s := range []string{"opencode:ses", "codex:abc", "plain-claude-uuid"} {
		if platform.IsCorpusSession(s) {
			t.Errorf("%q is a conversation, not a corpus", s)
		}
	}
}

func TestFilePlatformRegistered(t *testing.T) {
	// The blank-import side effect: a "file:"-prefixed session resolves to this platform.
	if _, ok := platform.ByName("file"); !ok {
		t.Fatal("file platform not registered via init()")
	}
}
