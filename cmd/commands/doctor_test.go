package commands

import (
	"strings"
	"testing"

	"github.com/IngTian/witness/internal/lens"
)

// modelTier ranks the known Claude families and abstains (ok=false) on anything it
// can't judge — a custom id, a Bedrock ARN, or "" (the runner's environment default).
func TestModelTier(t *testing.T) {
	cases := []struct {
		model   string
		wantOK  bool
		wantAbv string // a lower-tier model to compare against, for the ordering asserts below
	}{
		{"claude-haiku-4-5-20251001", true, ""},
		{"claude-sonnet-5", true, "claude-haiku-4-5-20251001"},
		{"claude-opus-4-8", true, "claude-sonnet-5"},
		{"", false, ""},                       // runner default — unrankable
		{"arn:aws:bedrock:custom", false, ""}, // opaque id — unrankable
		{"gpt-4o", false, ""},                 // non-Claude — unrankable
	}
	for _, c := range cases {
		tier, ok := modelTier(c.model)
		if ok != c.wantOK {
			t.Fatalf("modelTier(%q) ok=%v, want %v", c.model, ok, c.wantOK)
		}
		if ok && c.wantAbv != "" {
			lower, _ := modelTier(c.wantAbv)
			if !(tier > lower) {
				t.Fatalf("modelTier(%q)=%d must outrank modelTier(%q)=%d", c.model, tier, c.wantAbv, lower)
			}
		}
	}
}

// modelFloorWarnings warns ONLY when a lens's floor tier-ranks strictly above the
// (rankable) triage model — and never warns on an unrankable model on either side.
func TestModelFloorWarnings(t *testing.T) {
	arcSonnet := &lens.Lens{Name: "codereview", Kind: lens.KindArc, ModelFloor: "sonnet"}
	atomicNoFloor := &lens.Lens{Name: "default", Kind: lens.KindAtomic}
	all := []*lens.Lens{arcSonnet, atomicNoFloor}

	// Below floor (haiku < sonnet) → exactly one warning, naming the lens.
	w := modelFloorWarnings("claude-haiku-4-5-20251001", all)
	if len(w) != 1 || !strings.Contains(w[0], "codereview") {
		t.Fatalf("haiku under a sonnet floor must warn once about codereview, got %v", w)
	}

	// At/above floor (sonnet == sonnet, opus > sonnet) → no warning.
	if w := modelFloorWarnings("claude-sonnet-5", all); len(w) != 0 {
		t.Fatalf("triage at the floor must not warn, got %v", w)
	}
	if w := modelFloorWarnings("claude-opus-4-8", all); len(w) != 0 {
		t.Fatalf("triage above the floor must not warn, got %v", w)
	}

	// Unrankable triage model (runner default / custom id) → no basis to warn.
	if w := modelFloorWarnings("", all); len(w) != 0 {
		t.Fatalf("an unrankable triage model must not warn, got %v", w)
	}
	if w := modelFloorWarnings("arn:aws:bedrock:whatever", all); len(w) != 0 {
		t.Fatalf("an opaque triage id must not warn, got %v", w)
	}

	// A lens with no floor never warns, whatever the triage model.
	if w := modelFloorWarnings("claude-haiku-4-5-20251001", []*lens.Lens{atomicNoFloor}); len(w) != 0 {
		t.Fatalf("a floor-less lens must not warn, got %v", w)
	}
}
