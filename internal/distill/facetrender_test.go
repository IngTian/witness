package distill

import (
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/IngTian/witness/internal/store"
)

// facet builds one facet with len(values) versions, the last one open.
func facet(lens, dim, key string, conf float64, recordedAt string, values ...string) store.Facet {
	f := store.Facet{Lens: lens, Dimension: dim, Key: key}
	for i, v := range values {
		fv := store.FacetVersion{Value: v, Confidence: conf, RecordedAt: recordedAt}
		if i < len(values)-1 {
			fv.ValidTo = recordedAt // closed
		}
		f.Versions = append(f.Versions, fv)
	}
	return f
}

// The identity floor. applyFacet keys on lens|dimension|key, so a facet the model cannot see
// is not "omitted" — it is silently minted as a BRAND-NEW duplicate facet with its own history,
// and nothing logs it. So clipping may shrink values to nothing and must still emit every
// dimension/key, however tight the budget.
func TestEveryIdentitySurvivesEvenTheTightestClip(t *testing.T) {
	var facets []store.Facet
	for i := 0; i < 500; i++ {
		facets = append(facets, facet("default", fmt.Sprintf("dim%d", i%8), fmt.Sprintf("key_%d", i),
			0.7, "2026-10-01T00:00:00Z", strings.Repeat("x", 4000)))
	}
	text, _, indexN, _ := renderPriorFacets(facets, "default", 5, 0)
	if indexN != 500 {
		t.Fatalf("index count = %d, want 500", indexN)
	}
	for i := 0; i < 500; i++ {
		want := fmt.Sprintf("dim%d/key_%d", i%8, i)
		if !strings.Contains(text, want) {
			t.Fatalf("identity %q missing at clip=5 — applyFacet would mint a duplicate facet", want)
		}
	}
	if len(text) > 120_000 {
		t.Errorf("clip=5 still produced %d chars; the clip is not being applied", len(text))
	}
}

// Clip at a RUNE boundary. RenderChunks was deliberately fixed the same way: a byte budget is
// ~3x tighter for CJK, and this archive is part-Chinese, so a byte clip both under-fills and
// can split a rune into invalid UTF-8.
func TestClipsAtRuneBoundaryNotByteBoundary(t *testing.T) {
	facets := []store.Facet{facet("default", "thinking", "cjk", 0.5, "2026-10-01T00:00:00Z", "你好世界今天天气很好")}
	text, _, _, _ := renderPriorFacets(facets, "default", 4, 0)
	if !utf8.ValidString(text) {
		t.Fatal("clipping produced invalid UTF-8 — it cut mid-rune")
	}
	if !strings.Contains(text, "你好世界"+clipMarker) {
		t.Errorf("want the first 4 RUNES then the marker, got: %q", text)
	}
	if strings.Contains(text, "今天") {
		t.Error("clip let content past the budget through")
	}
}

// The full-prose tier is "has changed before", most recently changed first, capped by count —
// and shownFull must name exactly that set, because it is what the blind-close guard trusts.
func TestFullTierIsChangedFacetsMostRecentlyChangedFirst(t *testing.T) {
	long := strings.Repeat("y", 900)
	facets := []store.Facet{
		facet("default", "a", "never_changed", 0.5, "2026-01-01T00:00:00Z", long),
		facet("default", "b", "changed_old", 0.5, "2026-02-01T00:00:00Z", long, long+"old"),
		facet("default", "c", "changed_recent", 0.5, "2026-09-01T00:00:00Z", long, long+"recent"),
		facet("default", "d", "changed_mid", 0.5, "2026-05-01T00:00:00Z", long, long+"mid"),
	}
	text, shownFull, indexN, fullN := renderPriorFacets(facets, "default", 40, 2)

	if indexN != 4 {
		t.Errorf("index should still carry all 4 facets, got %d", indexN)
	}
	if fullN != 2 {
		t.Fatalf("full tier should be capped at 2, got %d", fullN)
	}
	// Capped to the two most recently changed; the oldest change and the never-changed one out.
	for _, want := range []string{"c|changed_recent", "d|changed_mid"} {
		if !shownFull[want] {
			t.Errorf("shownFull missing %q", want)
		}
	}
	for _, unwanted := range []string{"a|never_changed", "b|changed_old"} {
		if shownFull[unwanted] {
			t.Errorf("shownFull should not contain %q — it was never sent in full", unwanted)
		}
	}
	// Ordering inside the full block: recent before mid.
	full := text[strings.Index(text, "FULL CURRENT VALUE"):]
	if strings.Index(full, "changed_recent") > strings.Index(full, "changed_mid") {
		t.Error("full tier is not ordered most-recently-changed first")
	}
}

// clip <= 0 is the escape hatch: old unbounded behaviour, and crucially every facet counts as
// fully shown so the blind-close guard correctly stops firing (nothing was hidden).
func TestUnboundedModeSendsEverythingAndDisablesTheGuard(t *testing.T) {
	long := strings.Repeat("z", 3000)
	facets := []store.Facet{
		facet("default", "a", "one", 0.5, "2026-01-01T00:00:00Z", long),
		facet("default", "b", "two", 0.5, "2026-01-01T00:00:00Z", long),
	}
	text, shownFull, _, _ := renderPriorFacets(facets, "default", -1, 48)
	if strings.Contains(text, clipMarker) {
		t.Error("unbounded mode must not clip anything")
	}
	if len(shownFull) != 2 {
		t.Fatalf("unbounded mode must mark every facet fully shown, got %d", len(shownFull))
	}
	if !strings.Contains(text, long) {
		t.Error("unbounded mode should contain the full value")
	}
}

// The marker and the clip rule appear ONLY when something was actually clipped — otherwise the
// prompt would tell the model to distrust values that are in fact complete.
func TestNoMarkerOrClipRuleWhenNothingWasClipped(t *testing.T) {
	facets := []store.Facet{facet("default", "a", "short", 0.5, "2026-01-01T00:00:00Z", "brief")}
	text, _, _, _ := renderPriorFacets(facets, "default", 160, 48)
	if strings.Contains(text, clipMarker) {
		t.Error("nothing exceeded the clip, so no marker should appear")
	}
	if strings.Contains(text, "re-asked with that facet's full value") {
		t.Error("the clip rule should be omitted when no value was clipped")
	}
}

// Other lenses' facets must never leak into this lens's prompt — they would be offered as
// reusable keys and could be cross-wired into the wrong lens's history.
func TestOtherLensesAreExcluded(t *testing.T) {
	facets := []store.Facet{
		facet("default", "a", "mine", 0.5, "2026-01-01T00:00:00Z", "v"),
		facet("math", "a", "theirs", 0.5, "2026-01-01T00:00:00Z", "v"),
	}
	text, shownFull, indexN, _ := renderPriorFacets(facets, "default", 160, 48)
	if indexN != 1 || strings.Contains(text, "theirs") {
		t.Errorf("math's facet leaked into default's prompt: indexN=%d", indexN)
	}
	if shownFull["a|theirs"] {
		t.Error("shownFull leaked another lens's facet")
	}
}
