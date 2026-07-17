package distill

import (
	"strings"

	"github.com/IngTian/witness/internal/lens"
	"github.com/IngTian/witness/internal/store"
)

// Phase names the distillation stage a model is being chosen for. It exists so the
// per-lens/global mapping lives in ONE place (ModelFor) instead of each call site
// re-deciding "extract uses TriageModel, review/summary use DistillModel" — the lens
// package must not know which global config field backs which phase.
type Phase int

const (
	PhaseExtract Phase = iota // L0→L1 mining (per session; the dominant per-lens cost)
	PhaseReview               // L1→L2 review + L4 summary (batched)
)

// ModelFor resolves the model for a phase on a given lens (issue #75, slice 1): a
// lens's per-lens override wins, else the global stage model. Empty lens fields mean
// "ride the global", so a lens that declares nothing behaves exactly as before this
// change. ln may be nil (e.g. the cross-lens unified summary, which has no lens) — then
// only the global applies. This is the sole seam translating a phase into a config
// field, keeping that knowledge out of both the lens package and the call sites.
func ModelFor(cfg store.Config, ln *lens.Lens, phase Phase) string {
	global := cfg.TriageModel
	if phase == PhaseReview {
		global = cfg.DistillModel
	}
	if ln == nil {
		return global
	}
	override := ln.ExtractModel
	if phase == PhaseReview {
		override = ln.ReviewModel
	}
	if strings.TrimSpace(override) != "" {
		return override
	}
	return global
}
