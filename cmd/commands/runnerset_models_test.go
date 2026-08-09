package commands

import (
	"strings"
	"testing"

	"github.com/IngTian/witness/internal/store"
)

// A cross-runtime runner must not inherit the other runtime's model names.
//
// A model name is only valid on its own runtime, so handing an OpenCode runner the Claude-side
// default would make its Open-time validation reject a name that was never meant for it. The
// default runtime, by contrast, MUST keep its configured models — clearing those would silently
// downgrade every drain to the provider's default model.
func TestCrossRuntimeModelsAreClearedButTheDefaultRuntimeKeepsThem(t *testing.T) {
	base := store.Config{TriageModel: "triage-x", DistillModel: "distill-y"}

	// Same runtime as the default → keep.
	cfg := base
	clearCrossRuntimeModels(&cfg, "claude", "claude")
	if cfg.TriageModel != "triage-x" || cfg.DistillModel != "distill-y" {
		t.Errorf("the default runtime must keep its configured models, got triage=%q distill=%q; "+
			"clearing them silently downgrades every drain to the provider default",
			cfg.TriageModel, cfg.DistillModel)
	}

	// Different runtime → clear both.
	cfg = base
	clearCrossRuntimeModels(&cfg, "opencode", "claude")
	if cfg.TriageModel != "" || cfg.DistillModel != "" {
		t.Errorf("a cross-runtime runner must not inherit the default runtime's model names, got "+
			"triage=%q distill=%q; Open would reject a name meant for another runtime",
			cfg.TriageModel, cfg.DistillModel)
	}

	// Whitespace must not defeat the comparison (the config is user-edited text).
	cfg = base
	clearCrossRuntimeModels(&cfg, " claude ", "claude")
	if cfg.TriageModel != "triage-x" {
		t.Error("runtime names must be compared trimmed; ' claude ' is the default runtime and must " +
			"keep its models")
	}
}

// The model-union claim must not come back without an implementation (#148).
//
// The old applyModelUnion took a `lenses` parameter it never referenced, while its doc comment
// promised the runner validated "the UNION of that runtime's per-lens models". The claim described
// behavior that never existed — which is worse than an absent feature, because a reader who trusts
// it stops looking. This scan fails if that promise reappears in the file without the word
// documenting that it is deliberately not implemented.
func TestNoUnimplementedModelUnionClaim(t *testing.T) {
	src := readSource(t, "runnerset.go")
	i := strings.Index(src, "UNION of that runtime's per-lens models")
	if i < 0 {
		return // the claim is gone entirely; nothing to guard
	}
	// If the phrase is present, it must be in a passage that says it is NOT implemented.
	window := src[max0(i-800):min(len(src), i+800)]
	if !strings.Contains(window, "does NOT") && !strings.Contains(window, "NOT IMPLEMENTED") {
		t.Error("runnerset.go claims the runner validates the UNION of per-lens models. Either " +
			"implement it (StartOpenCodeServerIn already takes variadic models) or keep the note " +
			"saying it is deliberately not implemented — an unqualified claim stops anyone looking.")
	}
}
