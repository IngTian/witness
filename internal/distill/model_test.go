package distill

import (
	"testing"

	"github.com/IngTian/witness/internal/lens"
	"github.com/IngTian/witness/internal/store"
)

// ModelFor is the per-lens/default model resolver (#75): a lens's per-lens override wins
// for its phase, else the default stage model; a nil lens (the cross-lens unified summary)
// always takes the default. This is the whole behavior slice 1 adds.
func TestModelFor(t *testing.T) {
	cfg := store.Config{TriageModel: "default-triage", DistillModel: "default-distill"}

	// A lens that declares nothing rides the default stage model for each phase — the
	// pre-#75 behavior, unchanged.
	plain := &lens.Lens{Name: "plain"}
	if got := ModelFor(cfg, plain, PhaseExtract); got != "default-triage" {
		t.Fatalf("plain lens extract: want default-triage, got %q", got)
	}
	if got := ModelFor(cfg, plain, PhaseReview); got != "default-distill" {
		t.Fatalf("plain lens review: want default-distill, got %q", got)
	}

	// Per-lens overrides win for their own phase, independently.
	tuned := &lens.Lens{Name: "tuned", ExtractModel: "cheap-extract", ReviewModel: "strong-review"}
	if got := ModelFor(cfg, tuned, PhaseExtract); got != "cheap-extract" {
		t.Fatalf("tuned lens extract override ignored, got %q", got)
	}
	if got := ModelFor(cfg, tuned, PhaseReview); got != "strong-review" {
		t.Fatalf("tuned lens review override ignored, got %q", got)
	}

	// A per-lens override for ONLY one phase leaves the other on the default.
	extractOnly := &lens.Lens{Name: "eo", ExtractModel: "cheap-extract"}
	if got := ModelFor(cfg, extractOnly, PhaseExtract); got != "cheap-extract" {
		t.Fatalf("extract-only override ignored, got %q", got)
	}
	if got := ModelFor(cfg, extractOnly, PhaseReview); got != "default-distill" {
		t.Fatalf("extract-only override must not affect review, got %q", got)
	}

	// A whitespace-only override is treated as unset (rides the default).
	blank := &lens.Lens{Name: "b", ExtractModel: "   "}
	if got := ModelFor(cfg, blank, PhaseExtract); got != "default-triage" {
		t.Fatalf("whitespace override must ride the default, got %q", got)
	}

	// A nil lens (the unified cross-lens summary) always takes the default stage model.
	if got := ModelFor(cfg, nil, PhaseExtract); got != "default-triage" {
		t.Fatalf("nil lens extract: want default-triage, got %q", got)
	}
	if got := ModelFor(cfg, nil, PhaseReview); got != "default-distill" {
		t.Fatalf("nil lens review: want default-distill, got %q", got)
	}
}

// Review falls back to TriageModel when DistillModel is unset, so a single
// `triage_model` covers the whole pipeline and the review never silently inherits the
// runner's ambient default (for `claude -p`, the operator's heavy interactive model —
// which stalled the review during v0.3.0 live-test). An explicit DistillModel still wins.
func TestModelForReviewFallsBackToTriageWhenDistillEmpty(t *testing.T) {
	// Only triage_model set; distill_model empty.
	cfg := store.Config{Runner: "claude", TriageModel: "haiku", DistillModel: ""}
	if got := ModelFor(cfg, nil, PhaseReview); got != "haiku" {
		t.Fatalf("empty DistillModel review must fall back to TriageModel, got %q", got)
	}
	if got := ModelFor(cfg, &lens.Lens{Name: "d"}, PhaseReview); got != "haiku" {
		t.Fatalf("per-lens (no override) review must fall back to TriageModel, got %q", got)
	}
	// An explicit DistillModel still overrides the review stage.
	cfg2 := store.Config{Runner: "claude", TriageModel: "haiku", DistillModel: "sonnet"}
	if got := ModelFor(cfg2, nil, PhaseReview); got != "sonnet" {
		t.Fatalf("explicit DistillModel must win for review, got %q", got)
	}
	// Both empty → "" (the runner's own default), unchanged.
	cfg3 := store.Config{Runner: "claude"}
	if got := ModelFor(cfg3, nil, PhaseReview); got != "" {
		t.Fatalf("both empty → runner default (\"\"), got %q", got)
	}
	// Extract is unaffected by the fallback (still TriageModel directly).
	if got := ModelFor(cfg, nil, PhaseExtract); got != "haiku" {
		t.Fatalf("extract still uses TriageModel, got %q", got)
	}
}

// RunnerFor routes a lens to its own runner if set, else the default (cfg.Runner); a nil
// lens rides the default (#75 slice 2).
func TestRunnerFor(t *testing.T) {
	cfg := store.Config{Runner: "claude"}
	if got := RunnerFor(cfg, &lens.Lens{Name: "x"}); got != "claude" {
		t.Fatalf("lens with no runner rides the default, got %q", got)
	}
	if got := RunnerFor(cfg, &lens.Lens{Name: "x", Runner: "opencode"}); got != "opencode" {
		t.Fatalf("per-lens runner must win, got %q", got)
	}
	if got := RunnerFor(cfg, &lens.Lens{Name: "x", Runner: "  opencode  "}); got != "opencode" {
		t.Fatalf("per-lens runner must be trimmed, got %q", got)
	}
	if got := RunnerFor(cfg, nil); got != "claude" {
		t.Fatalf("nil lens rides the default, got %q", got)
	}
}

// ModelFor is runner-aware (#75 slice 2): a lens on a DIFFERENT runner than the default,
// with no per-lens model, must fall back to "" (its runtime's own default) — NOT the
// default stage model, which belongs to the wrong runtime. A per-lens model still wins
// regardless of runner.
func TestModelForCrossRuntime(t *testing.T) {
	cfg := store.Config{Runner: "claude", TriageModel: "claude-triage", DistillModel: "claude-distill"}

	// A lens routed to a DIFFERENT runner with no per-lens model → "" (opencode's default),
	// not the claude default (which would be a wrong-runtime model name).
	cross := &lens.Lens{Name: "cr", Runner: "opencode"}
	if got := ModelFor(cfg, cross, PhaseExtract); got != "" {
		t.Fatalf("cross-runtime lens with no model must ride its runtime default (\"\"), got %q", got)
	}
	if got := ModelFor(cfg, cross, PhaseReview); got != "" {
		t.Fatalf("cross-runtime lens review with no model must be \"\", got %q", got)
	}

	// A cross-runtime lens WITH a per-lens model uses that model.
	crossTuned := &lens.Lens{Name: "ct", Runner: "opencode", ExtractModel: "openai/free", ReviewModel: "openai/strong"}
	if got := ModelFor(cfg, crossTuned, PhaseExtract); got != "openai/free" {
		t.Fatalf("cross-runtime per-lens extract model ignored, got %q", got)
	}
	if got := ModelFor(cfg, crossTuned, PhaseReview); got != "openai/strong" {
		t.Fatalf("cross-runtime per-lens review model ignored, got %q", got)
	}

	// A lens on the SAME runner as the default (explicitly) still inherits the defaults.
	same := &lens.Lens{Name: "s", Runner: "claude"}
	if got := ModelFor(cfg, same, PhaseExtract); got != "claude-triage" {
		t.Fatalf("a lens on the default runner must inherit the default model, got %q", got)
	}
}
