package view

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/IngTian/witness/internal/store"
)

// TestRenderOrderAndCSP verifies the HTML structure: exact prefix (doctype, lang, translate,
// charset before CSP), CSP meta before first script, hash computed from the ACTUAL spliced JS
// bytes (not from pageJS, which could differ if Render added whitespace), and constant title
// that appears exactly once.
func TestRenderOrderAndCSP(t *testing.T) {
	m := Build(&store.ViewInput{}, nil, time.Now(), time.UTC)
	html, err := Render(m)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}

	s := string(html)

	// Exact prefix: doctype, lang, translate="no", charset, CSP. This pins translate="no" and
	// charset-first, so a reordering or omission fails cleanly instead of silently degrading.
	expectedPrefix := `<!doctype html><html lang="en" translate="no"><head><meta charset="utf-8"><meta http-equiv=`
	if !strings.HasPrefix(s, expectedPrefix) {
		t.Fatalf("output does not start with expected prefix\ngot:  %s...\nwant: %s...", s[:min(len(s), len(expectedPrefix)+20)], expectedPrefix)
	}

	// Title occurs exactly once
	titleCount := strings.Count(s, "<title")
	if titleCount != 1 {
		t.Fatalf("found %d <title occurrences, want 1", titleCount)
	}
	if !strings.Contains(s, "<title>witness</title>") {
		t.Error("title is not constant witness")
	}

	// Extract CSP content attribute
	cspIdx := strings.Index(s, `<meta http-equiv="Content-Security-Policy"`)
	if cspIdx == -1 {
		t.Fatal("missing CSP meta")
	}
	firstScriptIdx := strings.Index(s, "<script")
	if firstScriptIdx == -1 {
		t.Fatal("no <script> tag")
	}
	if firstScriptIdx < cspIdx {
		t.Fatal("first script before CSP meta")
	}

	cspMatch := regexp.MustCompile(`content="([^"]+)"`).FindStringSubmatch(s[cspIdx:firstScriptIdx])
	if cspMatch == nil {
		t.Fatal("no content= in CSP meta")
	}
	cspContent := cspMatch[1]

	// Extract the ACTUAL spliced JS bytes from the output. The second <script> (after the data
	// one) is the page.js script. If Render accidentally adds whitespace (leading space,
	// trailing newline), the browser would hash different bytes and block the script.
	dataScriptEnd := strings.Index(s, `</script>`)
	if dataScriptEnd == -1 {
		t.Fatal("no </script> end tag for data script")
	}
	secondScriptStart := strings.Index(s[dataScriptEnd:], "<script>")
	if secondScriptStart == -1 {
		t.Fatal("no second <script> tag for page.js")
	}
	secondScriptStart += dataScriptEnd + len("<script>")
	secondScriptEnd := strings.Index(s[secondScriptStart:], "</script>")
	if secondScriptEnd == -1 {
		t.Fatal("no </script> end tag for page.js")
	}
	actualJS := []byte(s[secondScriptStart : secondScriptStart+secondScriptEnd])

	// Hash the actual spliced bytes
	h := sha256.Sum256(actualJS)
	actualHash := base64.StdEncoding.EncodeToString(h[:])
	expectedCSP := "default-src 'none'; script-src 'sha256-" + actualHash + "'; style-src 'unsafe-inline'; base-uri 'none'; form-action 'none'"

	if cspContent != expectedCSP {
		t.Errorf("CSP mismatch\ngot:  %s\nwant: %s", cspContent, expectedCSP)
	}
}

// TestBundleGuards verifies that the embedded JS and CSS contain no forbidden patterns, and
// that Render's runtime guard (which uses the same package regexps) returns errBundle when
// given bad assets. The table test temporarily swaps pageJS/pageCSS with hostile content,
// calls Render, asserts errBundle, and restores the vars.
func TestBundleGuards(t *testing.T) {
	// Check that the real embedded assets pass the guards
	if bundleForbidden.Match(pageJS) {
		t.Error("page.js contains forbidden pattern (bundleForbidden)")
	}
	if bundleForbidden.Match(pageCSS) {
		t.Error("page.css contains forbidden pattern (bundleForbidden)")
	}
	if jsForbidden.Match(pageJS) {
		t.Error("page.js contains forbidden JS API (jsForbidden)")
	}

	// Render should not return errBundle for clean assets
	m := Build(&store.ViewInput{}, nil, time.Now(), time.UTC)
	_, err := Render(m)
	if errors.Is(err, errBundle) {
		t.Error("Render returned errBundle for clean assets")
	} else if err != nil {
		t.Fatalf("Render: %v", err)
	}

	// Table test: swap pageJS/pageCSS with bad bundles, assert Render returns errBundle.
	// Each case restores the original vars in t.Cleanup so subsequent tests see clean assets.
	badBundles := []struct {
		name    string
		js      []byte
		css     []byte
		wantErr bool
	}{
		{"innerHTML", []byte("el.innerHTML = x"), nil, true},
		{"dangerouslySetInnerHTML", []byte("el.dangerouslySetInnerHTML = x"), nil, true},
		{"createContextualFragment", []byte("range.createContextualFragment(x)"), nil, true},
		{"parseFromString", []byte("parser.parseFromString(x, 'text/html')"), nil, true},
		{"setHTMLUnsafe", []byte("el.setHTMLUnsafe(x)"), nil, true},
		{"srcdoc", []byte("iframe.srcdoc = x"), nil, true},
		{"location.href", []byte("location.href = x"), nil, true},
		{"location.assign", []byte("location.assign(x)"), nil, true},
		{"location.replace", []byte("location.replace(x)"), nil, true},
		{"</SCRIPT in JS", []byte("x = '</SCRIPT>'"), nil, true},
		{"</script in JS", []byte("x = '</script>'"), nil, true},
		{"<!-- in JS", []byte("<!-- comment"), nil, true},
		{"\\r in JS", []byte("var x = 1;\r\nvar y = 2;"), nil, true},
		{"</style in CSS", []byte("ignored"), []byte("</style>"), true},
		{"<!-- in CSS", []byte("ignored"), []byte("/* <!-- */"), true},
		{"\\r in CSS", []byte("ignored"), []byte("body {\r\n  margin: 0;\r\n}"), true},
		{"clean", []byte("document.body.textContent = 'ok'"), []byte("body { margin: 0; }"), false},
	}

	for _, tc := range badBundles {
		t.Run(tc.name, func(t *testing.T) {
			origJS := pageJS
			origCSS := pageCSS
			t.Cleanup(func() {
				pageJS = origJS
				pageCSS = origCSS
			})

			if tc.js != nil {
				pageJS = tc.js
			}
			if tc.css != nil {
				pageCSS = tc.css
			}

			_, err := Render(m)
			if tc.wantErr && !errors.Is(err, errBundle) {
				t.Errorf("expected errBundle, got %v", err)
			}
			if !tc.wantErr && err != nil {
				t.Errorf("expected no error, got %v", err)
			}
		})
	}
}

// TestRenderEscapesHostileText verifies that json.Marshal properly escapes dangerous characters
// in the model data. A Model with hostile text (end tags, HTML comments, bold, ampersand,
// Chinese, U+2028/U+2029 line/paragraph separators) renders, the output has only two
// case-insensitive </script occurrences (the real end tags), and parsing the JSON back returns
// the original string exactly.
func TestRenderEscapesHostileText(t *testing.T) {
	hostileText := "</script></SCRIPT><!--<b>& 中文  "

	// Create a Model with hostile text in a Trait's Summary field
	m := &Model{
		Snapshot: "2026-11-05T12:00:00Z",
		Activity: Activity{
			First:       "",
			Last:        "2026-11-05",
			Days:        []Day{},
			Cuts:        []int{},
			Windows:     []Window{},
			TotalTraits: 1,
		},
		Notebooks: []Notebook{},
		Traits: []Trait{
			{
				Lens:      "default",
				Dimension: "test",
				Key:       "hostile",
				Name:      "Hostile",
				Summary:   hostileText,
				Notes:     0,
				Arrival:   "2026-11-05",
				Today:     0.95,
				Points:    []Point{},
				Spans:     []Span{},
				Trend:     "only",
				TrendFrom: 0.95,
				Rewrites:  0,
				Changes:   0,
				Formed:    nil,
			},
		},
		Portraits: []Portrait{},
	}

	html, err := Render(m)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}

	s := string(html)

	// Count case-insensitive </script occurrences: should be exactly 2 (data script end tag,
	// page.js script end tag). The hostile text contains "</script></SCRIPT>" but json.Marshal
	// escapes < and > so they cannot break out.
	count := len(regexp.MustCompile(`(?i)</script`).FindAllString(s, -1))
	if count != 2 {
		t.Errorf("found %d case-insensitive </script occurrences, want 2", count)
	}

	// Extract JSON from #data and parse it back
	dataStart := strings.Index(s, `<script type="application/json" id="data">`)
	if dataStart == -1 {
		t.Fatal("no data script")
	}
	dataStart += len(`<script type="application/json" id="data">`)
	dataEnd := strings.Index(s[dataStart:], "</script>")
	if dataEnd == -1 {
		t.Fatal("no data script end tag")
	}
	jsonStr := s[dataStart : dataStart+dataEnd]

	var parsed Model
	if err := json.Unmarshal([]byte(jsonStr), &parsed); err != nil {
		t.Fatalf("JSON parse failed: %v", err)
	}

	// Verify the hostile text round-trips exactly
	if len(parsed.Traits) != 1 {
		t.Fatalf("expected 1 trait, got %d", len(parsed.Traits))
	}
	if parsed.Traits[0].Summary != hostileText {
		t.Errorf("summary mismatch\ngot:  %q\nwant: %q", parsed.Traits[0].Summary, hostileText)
	}
}

// TestRenderNoNullSlices verifies that an empty Build produces JSON with no null values except
// for the allowed busiest/formed fields.
func TestRenderNoNullSlices(t *testing.T) {
	m := Build(&store.ViewInput{}, nil, time.Now(), time.UTC)
	html, err := Render(m)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}

	s := string(html)

	// Extract JSON from #data
	dataStart := strings.Index(s, `<script type="application/json" id="data">`)
	if dataStart == -1 {
		t.Fatal("no data script")
	}
	dataStart += len(`<script type="application/json" id="data">`)
	dataEnd := strings.Index(s[dataStart:], "</script>")
	if dataEnd == -1 {
		t.Fatal("no data script end tag")
	}
	jsonStr := s[dataStart : dataStart+dataEnd]

	// Check for null occurrences - allowed only in busiest/formed context
	nullMatches := regexp.MustCompile(`:\s*null`).FindAllStringIndex(jsonStr, -1)
	for _, match := range nullMatches {
		// Get context around this null
		start := match[0] - 20
		if start < 0 {
			start = 0
		}
		end := match[1] + 20
		if end > len(jsonStr) {
			end = len(jsonStr)
		}
		context := jsonStr[start:end]

		// Allow null only for busiest or formed
		if !strings.Contains(context, "busiest") && !strings.Contains(context, "formed") {
			t.Errorf("unexpected null at position %d: %s", match[0], context)
		}
	}
}
