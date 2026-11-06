package view

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"

	_ "embed"
)

//go:embed assets/page.js
var pageJS []byte

//go:embed assets/page.css
var pageCSS []byte

// errBundle is returned when the embedded JS/CSS bundle violates a guard. The guard runs at
// render time and is also checked by TestBundleGuards.
var errBundle = errors.New("embedded bundle violates guard")

// bundleForbidden matches patterns forbidden in both JS and CSS: case-insensitive </script,
// HTML comments, and carriage returns. These would break the single-file construction or leak
// through json.Marshal's escaping.
var bundleForbidden = regexp.MustCompile(`(?i)</script|<!--|` + "\r")

// jsForbidden matches APIs forbidden in page.js: DOM APIs that bypass textContent (innerHTML,
// outerHTML, insertAdjacentHTML, document.write), eval, storage APIs (localStorage,
// sessionStorage, indexedDB), and navigation/history APIs (location.hash, pushState,
// replaceState, document.title assignment). All state must live in memory only, and all
// rendering must be safe by construction.
var jsForbidden = regexp.MustCompile(`innerHTML|outerHTML|insertAdjacentHTML|document\.write|eval\(|localStorage|sessionStorage|indexedDB|location\.hash|pushState|replaceState|document\.title\s*=`)

// Render builds a single self-contained HTML file from the Model. The output is a complete
// HTML5 document with a hash-based CSP that allows only the embedded script to run. The Model
// is serialized to JSON in a <script type="application/json" id="data"> element, which
// json.Marshal safely escapes (< > & U+2028 U+2029). The JS bundle is spliced as plain bytes
// (never through html/template, which would re-quote and break the hash), and the CSP meta's
// script-src uses the sha256 of those exact bytes so no other script can execute.
//
// The file is opened from file:// and holds personal archive text (model output over pasted
// content), so it must be safe by construction: no inline event handlers, no dynamic script
// injection, no storage or navigation APIs that could leak state.
//
// Returns errBundle if the embedded assets violate a guard (checked at render time and by test).
func Render(m *Model) ([]byte, error) {
	// Guard: check embedded bundle at render time. The same patterns are checked by
	// TestBundleGuards. This runs on every call so a corrupted embed (dev error, supply-chain
	// attack, or a mutation that bypassed CI) cannot produce an unsafe file.
	if bundleForbidden.Match(pageJS) || bundleForbidden.Match(pageCSS) {
		return nil, errBundle
	}
	if jsForbidden.Match(pageJS) {
		return nil, errBundle
	}

	// Marshal the model to JSON. json.Marshal escapes < > & U+2028 U+2029, so hostile text in
	// the Model (e.g. "</script><!--") cannot break out of the <script> tag or inject HTML.
	dataJSON, err := json.Marshal(m)
	if err != nil {
		return nil, fmt.Errorf("marshal model: %w", err)
	}

	// Compute sha256 of the exact page.js bytes for the CSP. The hash locks the page to this
	// script: no other script (inline or injected) can run.
	h := sha256.Sum256(pageJS)
	scriptHash := base64.StdEncoding.EncodeToString(h[:])

	// Build the HTML. Order matters: charset meta first (so the parser uses UTF-8), CSP meta
	// before the first <script> (so the policy applies before any script runs), data script
	// before page.js (so page.js can read #data on load). The JS bundle is spliced as plain
	// bytes, never through html/template (which would escape quotes and break the hash).
	var html []byte
	html = append(html, []byte(`<!doctype html><html lang="en" translate="no"><head><meta charset="utf-8">`)...)
	html = append(html, []byte(`<meta http-equiv="Content-Security-Policy" content="default-src 'none'; script-src 'sha256-`)...)
	html = append(html, []byte(scriptHash)...)
	html = append(html, []byte(`'; style-src 'unsafe-inline'; base-uri 'none'; form-action 'none'">`)...)
	html = append(html, []byte(`<title>witness</title><style>`)...)
	html = append(html, pageCSS...)
	html = append(html, []byte(`</style></head><body><script type="application/json" id="data">`)...)
	html = append(html, dataJSON...)
	html = append(html, []byte(`</script><script>`)...)
	html = append(html, pageJS...)
	html = append(html, []byte(`</script></body></html>`)...)

	return html, nil
}
