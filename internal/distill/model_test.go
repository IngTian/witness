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

// RunnerFor routes a lens to its own runner if set, else the global (cfg.Runner); a nil
// lens rides the global (#75 slice 2).
func TestRunnerFor(t *testing.T) {
	cfg := store.Config{Runner: "claude"}
	if got := RunnerFor(cfg, &lens.Lens{Name: "x"}); got != "claude" {
		t.Fatalf("lens with no runner rides the global, got %q", got)
	}
	if got := RunnerFor(cfg, &lens.Lens{Name: "x", Runner: "opencode"}); got != "opencode" {
		t.Fatalf("per-lens runner must win, got %q", got)
	}
	if got := RunnerFor(cfg, &lens.Lens{Name: "x", Runner: "  opencode  "}); got != "opencode" {
		t.Fatalf("per-lens runner must be trimmed, got %q", got)
	}
	if got := RunnerFor(cfg, nil); got != "claude" {
		t.Fatalf("nil lens rides the global, got %q", got)
	}
}

// ModelFor is runner-aware (#75 slice 2): a lens on a DIFFERENT runner than the global,
// with no per-lens model, must fall back to "" (its runtime's own default) — NOT the
// global stage model, which belongs to the wrong runtime. A per-lens model still wins
// regardless of runner.
func TestModelForCrossRuntime(t *testing.T) {
	cfg := store.Config{Runner: "claude", TriageModel: "claude-triage", DistillModel: "claude-distill"}

	// A lens routed to a DIFFERENT runner with no per-lens model → "" (opencode's default),
	// not the claude global (which would be a wrong-runtime model name).
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

	// A lens on the SAME runner as the global (explicitly) still inherits the globals.
	same := &lens.Lens{Name: "s", Runner: "claude"}
	if got := ModelFor(cfg, same, PhaseExtract); got != "claude-triage" {
		t.Fatalf("a lens on the global runner must inherit the global model, got %q", got)
	}
}
