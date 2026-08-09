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

// The dead `lenses` parameter must not come back (#148).
//
// applyModelUnion took a `lenses []*lens.Lens` it never referenced, while its doc promised the runner
// validated "the UNION of that runtime's per-lens models" — behavior that never existed. Renaming it
// to clearCrossRuntimeModels and dropping the parameter is the fix; this guards the shape.
//
// IT GUARDS THE SIGNATURE, not a comment phrase. My first version searched the source for that
// promise string and returned early when absent — and the same commit that removed the promise made
// the search miss, so the assertion could never run. `grep -c "UNION of that runtime's per-lens
// models" runnerset.go` returned 0 immediately after I wrote it: a test about unimplemented promises
// that was itself a dead promise, written one hour after I documented this exact failure mode.
//
// A signature is the right thing to pin because it is what the defect WAS: a parameter that exists
// only to be ignored. Prose can be reworded; `func clearCrossRuntimeModels(rcfg, name, defaultRunner)`
// either takes lenses or it does not.
func TestModelClearingTakesNoLensesParameter(t *testing.T) {
	src := readSource(t, "runnerset.go")

	i := strings.Index(src, "func clearCrossRuntimeModels(")
	if i < 0 {
		t.Fatal("clearCrossRuntimeModels is gone; if it was renamed, re-point this guard at the new " +
			"name rather than deleting it — the invariant is that model clearing takes no lens list")
	}
	sig := src[i : i+strings.Index(src[i:], ")")+1]
	if strings.Contains(sig, "lens.") || strings.Contains(sig, "lenses") {
		t.Errorf("model clearing must NOT take a lens list: %s\n"+
			"That parameter existed for a per-lens model UNION that was never computed. Either "+
			"implement the union (StartOpenCodeServerIn already accepts variadic models) or keep the "+
			"parameter out — a parameter that exists only to be ignored is how the original defect "+
			"survived review.", sig)
	}

	// openRuntime carried the same dead parameter, purely to forward it here.
	j := strings.Index(src, "func (rs *runnerSet) openRuntime(")
	if j < 0 {
		t.Fatal("openRuntime not found")
	}
	osig := src[j : j+strings.Index(src[j:], ")")+1]
	if strings.Contains(osig, "lenses") {
		t.Errorf("openRuntime must not take a lens list either — it only ever forwarded it to the "+
			"clearing function: %s", osig)
	}
}
