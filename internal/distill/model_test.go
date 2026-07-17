package distill

import (
	"testing"

	"github.com/IngTian/witness/internal/lens"
	"github.com/IngTian/witness/internal/store"
)

// ModelFor is the per-lens/global model resolver (#75): a lens's per-lens override wins
// for its phase, else the global stage model; a nil lens (the cross-lens unified summary)
// always takes the global. This is the whole behavior slice 1 adds.
func TestModelFor(t *testing.T) {
	cfg := store.Config{TriageModel: "global-triage", DistillModel: "global-distill"}

	// A lens that declares nothing rides the global stage model for each phase — the
	// pre-#75 behavior, unchanged.
	plain := &lens.Lens{Name: "plain"}
	if got := ModelFor(cfg, plain, PhaseExtract); got != "global-triage" {
		t.Fatalf("plain lens extract: want global-triage, got %q", got)
	}
	if got := ModelFor(cfg, plain, PhaseReview); got != "global-distill" {
		t.Fatalf("plain lens review: want global-distill, got %q", got)
	}

	// Per-lens overrides win for their own phase, independently.
	tuned := &lens.Lens{Name: "tuned", ExtractModel: "cheap-extract", ReviewModel: "strong-review"}
	if got := ModelFor(cfg, tuned, PhaseExtract); got != "cheap-extract" {
		t.Fatalf("tuned lens extract override ignored, got %q", got)
	}
	if got := ModelFor(cfg, tuned, PhaseReview); got != "strong-review" {
		t.Fatalf("tuned lens review override ignored, got %q", got)
	}

	// A per-lens override for ONLY one phase leaves the other on the global.
	extractOnly := &lens.Lens{Name: "eo", ExtractModel: "cheap-extract"}
	if got := ModelFor(cfg, extractOnly, PhaseExtract); got != "cheap-extract" {
		t.Fatalf("extract-only override ignored, got %q", got)
	}
	if got := ModelFor(cfg, extractOnly, PhaseReview); got != "global-distill" {
		t.Fatalf("extract-only override must not affect review, got %q", got)
	}

	// A whitespace-only override is treated as unset (rides the global).
	blank := &lens.Lens{Name: "b", ExtractModel: "   "}
	if got := ModelFor(cfg, blank, PhaseExtract); got != "global-triage" {
		t.Fatalf("whitespace override must ride the global, got %q", got)
	}

	// A nil lens (the unified cross-lens summary) always takes the global stage model.
	if got := ModelFor(cfg, nil, PhaseExtract); got != "global-triage" {
		t.Fatalf("nil lens extract: want global-triage, got %q", got)
	}
	if got := ModelFor(cfg, nil, PhaseReview); got != "global-distill" {
		t.Fatalf("nil lens review: want global-distill, got %q", got)
	}
}
