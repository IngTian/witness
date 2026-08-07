package embed

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"
)

// The NFD fix, guarded WITHOUT the model — so it runs in CI.
//
// This is the second attempt at guarding it. The first was the model-loading test below, which
// silently skipped everywhere; pointing it at the repo's model fixed my machine and NOT CI,
// because assets/e5-small is gitignored (.gitignore:37-39, `git ls-files assets` = 0 files) and
// ci.yml has no fetch-model step — its comment even says "No model or `claude` CLI needed". So
// the v0.7.2 critical still had zero automated protection: a skip on a precondition CI never
// satisfies is a deleted test that reads as green, and moving which precondition it was did not
// change that.
//
// The durable fix is to test the seam that actually holds the property. Embed's normalization is
// a pure string transform; only the tokenizer needs the model. Asserting on it directly needs no
// assets, no network, and no 448MB download, so it cannot skip.
func TestSanitizeForTokenizerFoldsNFDAndInvalidBytes(t *testing.T) {
	// Written as explicit escapes, NOT as pasted literals. A decomposed literal in Go source is
	// fragile — an editor, a normalizing paste, or a formatter can compose it, after which the
	// test still passes while comparing NFC to NFC and proving nothing. That happened while
	// writing this test: the "NFD" literal I typed arrived as 63 61 66 c3a9 (already NFC).
	const nfc = "caf\u00e9"  // precomposed e-acute
	const nfd = "cafe\u0301" // e + combining acute — what macOS pastes produce
	if nfc == nfd {
		t.Fatal("fixture is broken: the NFC and NFD forms are byte-identical, so this test is vacuous")
	}
	if len(nfd) != 6 || len(nfc) != 5 {
		t.Fatalf("fixture is broken: want NFD=6 bytes and NFC=5, got %d and %d", len(nfd), len(nfc))
	}
	if got, want := sanitizeForTokenizer(nfd), nfc; got != want {
		t.Errorf("NFD was not composed: got %q (% x), want %q (% x)", got, got, want, want)
	}
	// NFC input must be untouched — normalization is idempotent, not lossy.
	if got := sanitizeForTokenizer(nfc); got != nfc {
		t.Errorf("NFC input was altered: got %q, want %q", got, nfc)
	}
	// Both forms must converge, which is what makes a pasted query match a stored observation.
	if sanitizeForTokenizer(nfc) != sanitizeForTokenizer(nfd) {
		t.Error("NFC and NFD of the same word must sanitize to the same string, or a macOS-pasted " +
			"query cannot match an NFC-stored observation")
	}

	// Invalid bytes must be replaced, not preserved: the tokenizer indexes by byte offset and
	// panics on them. "caf\xe9" is latin-1 "café", the shape that arrives from imported documents.
	if got := sanitizeForTokenizer("caf\xe9"); !utf8.ValidString(got) {
		t.Errorf("invalid UTF-8 survived sanitization: %q (% x)", got, got)
	}
	if got := sanitizeForTokenizer("caf\xe9"); !strings.Contains(got, "�") {
		t.Errorf("expected the replacement char for an invalid byte, got %q", got)
	}
	// Degenerate inputs must not blow up.
	for _, s := range []string{"", "\x00", "\xff\xfe", strings.Repeat("é", 1000)} {
		if got := sanitizeForTokenizer(s); !utf8.ValidString(got) {
			t.Errorf("sanitizeForTokenizer(%q) produced invalid UTF-8: %q", s, got)
		}
	}
}

// useRepoModel points the embedder at the model committed in the repo.
//
// The test below is the END-TO-END half: it proves the real tokenizer actually survives these
// inputs, which the pure-string test above cannot. It requires the model and therefore skips in
// CI — that is now acceptable, because the property is pinned unconditionally above. Keep BOTH:
// this one is what would catch the tokenizer itself regressing.
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
	const nfcCafe = "caf\u00e9"  // precomposed
	const nfdCafe = "cafe\u0301" // decomposed (macOS form); escaped so a normalizing
	//                                editor cannot silently turn this into a second NFC copy
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
