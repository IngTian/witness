package embed

import (
	"os"
	"path/filepath"
	"testing"
)

// useRepoModel points the embedder at the model committed in the repo.
//
// Without it this test SILENTLY SKIPPED EVERYWHERE — the NFD panic fix (a v0.7.2 critical: an
// NFD paste crashed `observations search` and killed the long-lived MCP server) had zero
// automated protection. The cause was mundane: New() resolves "assets/e5-small" relative to the
// process cwd, and `go test` runs with the PACKAGE dir as cwd, so from internal/embed/ the path
// missed a model that was sitting right there in the repo root. The skip then swallowed it.
//
// It returns false only if the model genuinely is not in the checkout, which is the one case
// where skipping is honest.
func useRepoModel(t *testing.T) bool {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		return false
	}
	dir := filepath.Join(root, "assets", "e5-small")
	if _, err := os.Stat(filepath.Join(dir, "model.onnx")); err != nil {
		return false
	}
	t.Setenv("WITNESS_ASSETS", dir)
	return true
}

// The tokenizer indexes by byte offset and PANICS on NFD-decomposed text — which is what
// macOS filesystems and pastes produce — and on non-UTF-8 bytes. The mining path has a
// recover barrier (distill/drain.go) but the READ paths did not, so `witness observations
// search` died with a Go stack trace and the long-lived MCP server process was killed
// mid-session by a panic on its jsonrpc2 handler goroutine. Embed now normalizes to NFC,
// replaces invalid bytes, and recovers as a backstop.
func TestEmbedHandlesNFDAndInvalidUTF8(t *testing.T) {
	if !useRepoModel(t) {
		t.Skip("no embedding model in this checkout (assets/e5-small); run scripts/fetch-model.sh")
	}
	e, err := New()
	if err != nil {
		t.Fatalf("the model is present, so New() must succeed — a skip here would hide the "+
			"NFD panic regression this test exists to catch: %v", err)
	}
	const nfcCafe = "café"  // é precomposed
	const nfdCafe = "café" // e + combining acute (macOS form)
	for name, s := range map[string]string{
		"NFC":          nfcCafe,
		"NFD":          nfdCafe,
		"NFD longer":   "résumé",
		"invalid UTF8": "caf\xe9",
		"empty":        "",
	} {
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("%s: Embed must not panic, got %v", name, r)
				}
			}()
			v, err := e.Embed(s)
			if err != nil {
				t.Errorf("%s: unexpected error: %v", name, err)
				return
			}
			if len(v) != Dim {
				t.Errorf("%s: got %d dims, want %d", name, len(v), Dim)
			}
		}()
	}

	// Normalization means the two forms are the SAME text, so they must embed identically
	// — a macOS-pasted query has to match an NFC-stored observation, not just avoid dying.
	a, err := e.Embed(nfcCafe)
	if err != nil {
		t.Fatal(err)
	}
	b, err := e.Embed(nfdCafe)
	if err != nil {
		t.Fatal(err)
	}
	for i := range a {
		if a[i] != b[i] {
			t.Fatalf("NFC and NFD of the same word must embed identically; differ at %d (%v vs %v)", i, a[i], b[i])
		}
	}
}
