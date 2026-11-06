package view

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/IngTian/witness/internal/store"
)

// TestRenderOrderAndCSP verifies the HTML structure: charset meta before CSP meta before first
// script, CSP exactly matches the global-constraints string with the hash recomputed, and title
// is constant.
func TestRenderOrderAndCSP(t *testing.T) {
	m := Build(&store.ViewInput{}, nil, time.Now(), time.UTC)
	html, err := Render(m)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}

	s := string(html)

	// charset meta before CSP meta
	charsetIdx := strings.Index(s, `<meta charset="utf-8">`)
	if charsetIdx == -1 {
		t.Fatal("missing <meta charset=\"utf-8\">")
	}

	cspIdx := strings.Index(s, `<meta http-equiv="Content-Security-Policy"`)
	if cspIdx == -1 {
		t.Fatal("missing CSP meta")
	}
	if cspIdx < charsetIdx {
		t.Error("CSP meta before charset meta")
	}

	// CSP before first script
	firstScriptIdx := strings.Index(s, "<script")
	if firstScriptIdx == -1 {
		t.Fatal("no <script> tag")
	}
	if firstScriptIdx < cspIdx {
		t.Error("first script before CSP meta")
	}

	// CSP string matches expected format with hash
	// Expected: default-src 'none'; script-src 'sha256-<hash>'; style-src 'unsafe-inline'; base-uri 'none'; form-action 'none'
	cspMatch := regexp.MustCompile(`content="([^"]+)"`).FindStringSubmatch(s[cspIdx:firstScriptIdx])
	if cspMatch == nil {
		t.Fatal("no content= in CSP meta")
	}
	cspContent := cspMatch[1]

	// Compute expected hash from embedded JS
	h := sha256.Sum256(pageJS)
	expectedHash := base64.StdEncoding.EncodeToString(h[:])
	expectedCSP := "default-src 'none'; script-src 'sha256-" + expectedHash + "'; style-src 'unsafe-inline'; base-uri 'none'; form-action 'none'"

	if cspContent != expectedCSP {
		t.Errorf("CSP mismatch\ngot:  %s\nwant: %s", cspContent, expectedCSP)
	}

	// title is constant
	if !strings.Contains(s, "<title>witness</title>") {
		t.Error("title is not constant witness")
	}
}

// TestBundleGuards verifies the embedded JS and CSS contain no forbidden patterns. The same
// guard runs at render time and returns errBundle.
func TestBundleGuards(t *testing.T) {
	// The guard checks both JS and CSS for these patterns
	forbidden := []struct {
		name string
		re   *regexp.Regexp
	}{
		{"</script (case-insensitive)", regexp.MustCompile(`(?i)</script`)},
		{"HTML comment", regexp.MustCompile(`<!--`)},
		{"carriage return", regexp.MustCompile(`\r`)},
	}

	for _, f := range forbidden {
		if f.re.Match(pageJS) {
			t.Errorf("page.js contains forbidden pattern: %s", f.name)
		}
		if f.re.Match(pageCSS) {
			t.Errorf("page.css contains forbidden pattern: %s", f.name)
		}
	}

	// JS-only forbidden patterns
	jsForbidden := regexp.MustCompile(`innerHTML|outerHTML|insertAdjacentHTML|document\.write|eval\(|localStorage|sessionStorage|indexedDB|location\.hash|pushState|replaceState|document\.title\s*=`)
	if jsForbidden.Match(pageJS) {
		t.Error("page.js contains forbidden JS API")
	}

	// Render should also check and return errBundle
	// We'll test this by trying to render (which should succeed with clean assets)
	m := Build(&store.ViewInput{}, nil, time.Now(), time.UTC)
	_, err := Render(m)
	if err == errBundle {
		t.Error("Render returned errBundle for clean assets")
	} else if err != nil {
		t.Fatalf("Render: %v", err)
	}
}

// TestRenderEscapesHostileText verifies that json.Marshal properly escapes dangerous characters
// in the model data. A Model with hostile text renders, the output has only two </script
// occurrences (the real end tags), and parsing the JSON back returns the original string.
func TestRenderEscapesHostileText(t *testing.T) {
	hostileText := `</script><!--<b>& 中文`

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

	// Count </script occurrences: should be exactly 2 (data script end tag, page.js script end tag)
	count := strings.Count(s, "</script")
	if count != 2 {
		t.Errorf("found %d </script occurrences, want 2", count)
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
