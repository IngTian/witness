package distill

import (
	"strings"

	"github.com/IngTian/witness/internal/lens"
	"github.com/IngTian/witness/internal/store"
)

// Phase names the distillation stage a model is being chosen for. It exists so the
// per-lens/default mapping lives in ONE place (ModelFor) instead of each call site
// re-deciding "extract uses TriageModel, review/summary use DistillModel" — the lens
// package must not know which default-config field backs which phase.
type Phase int

const (
	PhaseExtract Phase = iota // L0→L1 mining (per session; the dominant per-lens cost)
	PhaseReview               // L1→L2 review + L4 summary (batched)
)

// RunnerFor resolves which RUNTIME a lens runs on (issue #75 slice 2): the lens's own
// Runner if it declares one, else the default runner (cfg.Runner, which the worker sets to
// the resolved runner for this drain). A nil lens (the cross-lens unified summary) runs on
// the default runner. This is the sole place the lens→runtime routing decision lives.
func RunnerFor(cfg store.Config, ln *lens.Lens) string {
	if ln != nil && strings.TrimSpace(ln.Runner) != "" {
		return strings.TrimSpace(ln.Runner)
	}
	return strings.TrimSpace(cfg.Runner)
}

// ModelFor resolves the model for a phase on a given lens (issue #75). A lens's per-lens
// model override always wins. Otherwise the fallback depends on WHICH RUNNER the lens
// runs on:
//   - lens on the default runner (no per-lens Runner, or one equal to cfg.Runner) → the
//     default stage model (TriageModel for extract, DistillModel for review) — the slice-1
//     behavior, unchanged.
//   - lens on a DIFFERENT runner (slice 2) → "" (the runner's OWN default). The default
//     stage model belongs to the default runtime; handing e.g. a claude model name to an
//     OpenCode lens would be a bad model. So a cross-runtime lens with no per-lens model
//     rides its runtime's default rather than inheriting a wrong-runtime name.
//
// ln may be nil (unified summary) → the default stage model on the default runner.
func ModelFor(cfg store.Config, ln *lens.Lens, phase Phase) string {
	override := ""
	if ln != nil {
		override = ln.ExtractModel
		if phase == PhaseReview {
			override = ln.ReviewModel
		}
	}
	if strings.TrimSpace(override) != "" {
		return strings.TrimSpace(override)
	}
	// No per-lens model. The default stage model only applies when the lens runs on the
	// default runner; a cross-runtime lens falls back to its own runtime's default ("").
	if RunnerFor(cfg, ln) != strings.TrimSpace(cfg.Runner) {
		return ""
	}
	if phase == PhaseReview {
		// Review/summary rides DistillModel, but falls back to TriageModel when it's
		// unset — so a single `triage_model` covers the WHOLE pipeline. Without this, an
		// empty DistillModel means the review call omits --model and inherits the runner's
		// ambient default, which for `claude -p` is the operator's interactive `claude`
		// model (commonly a heavy frontier/Bedrock model): the batched review then runs
		// far slower/pricier than the mine — and can ride the 10-min timeout — even though
		// the user only ever set a light triage_model. Distillation latency/cost must not
		// be silently hostage to the ambient interactive default; an explicit distill_model
		// still overrides. (Both empty → "" → the runner's own default, as before.)
		if strings.TrimSpace(cfg.DistillModel) != "" {
			return cfg.DistillModel
		}
		return cfg.TriageModel
	}
	return cfg.TriageModel
}
