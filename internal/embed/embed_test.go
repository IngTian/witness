package embed

import "testing"

// The tokenizer indexes by byte offset and PANICS on NFD-decomposed text — which is what
// macOS filesystems and pastes produce — and on non-UTF-8 bytes. The mining path has a
// recover barrier (distill/drain.go) but the READ paths did not, so `witness observations
// search` died with a Go stack trace and the long-lived MCP server process was killed
// mid-session by a panic on its jsonrpc2 handler goroutine. Embed now normalizes to NFC,
// replaces invalid bytes, and recovers as a backstop.
func TestEmbedHandlesNFDAndInvalidUTF8(t *testing.T) {
	e, err := New()
	if err != nil {
		t.Skipf("model not available: %v", err)
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
