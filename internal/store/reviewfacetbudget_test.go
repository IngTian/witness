package store

import "testing"

// The prior-facet half of the review call MUST be bounded by default. Unbounded is not a
// neutral starting point — it is the exact payload that refused a real 2,039-facet lens
// outright (`claude -p` exit 1) and left it unable to fold for 24 days, with 2,107
// observations stranded and every lens's narrative frozen behind it.
//
// A <=0 value is still honoured from config.toml, deliberately, as an escape hatch for a lens
// that genuinely needs whole values. But it must never be reached by DEFAULT: that would ship
// the wedge to every new archive, and because the failure is a silent per-window error behind
// an advancing cadence, nobody would notice for weeks. This pins the default so a future edit
// to DefaultConfig cannot quietly reintroduce it.
func TestReviewFacetBudgetIsBoundedByDefault(t *testing.T) {
	cfg := DefaultConfig()

	if cfg.ReviewFacetClip <= 0 {
		t.Fatalf("DefaultConfig().ReviewFacetClip = %d; a non-positive default sends every "+
			"facet value in full, which is the 24-day wedge", cfg.ReviewFacetClip)
	}
	if cfg.ReviewFacetFull < 0 {
		t.Errorf("DefaultConfig().ReviewFacetFull = %d; must not be negative", cfg.ReviewFacetFull)
	}
	// The clip has to be wide enough to carry a change verdict. Measured on a real archive,
	// value changes diverge from their predecessor within 62 chars on one lens and 240 on
	// another; a default below ~100 would start hiding real changes behind the re-ask on
	// every pass, turning the bounded path into a two-call path for no reason.
	if cfg.ReviewFacetClip < 100 {
		t.Errorf("DefaultConfig().ReviewFacetClip = %d is too tight to carry a change verdict "+
			"(measured: changes diverge within 62-240 chars)", cfg.ReviewFacetClip)
	}
}
