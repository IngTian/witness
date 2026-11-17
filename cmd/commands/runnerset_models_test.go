package commands

import (
	"context"
	"slices"
	"testing"

	"github.com/IngTian/witness/internal/lens"
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

// runtimeModels must return both stages of every lens routed to the given runtime, plus on the
// default runtime the cross-lens unified summary's review model. Empty models ("") are kept and
// the list is deduped and sorted (four models: map order passes as sorted 1 time in 24).
func TestRuntimeModelsCoversEveryStageOnItsRuntime(t *testing.T) {
	cfg := store.Config{Runner: "opencode", TriageModel: "opencode/a"}
	ls := []*lens.Lens{
		{Name: "default"},
		{Name: "x", Runner: "claude"},
		{Name: "y", Runner: "opencode", ExtractModel: "opencode/b"},
		{Name: "z", Runner: "opencode", ExtractModel: "opencode/d", ReviewModel: "opencode/c"},
	}

	// y's review falls back to triage on the default runtime.
	if oc := runtimeModels(cfg, ls, "opencode"); !slices.Equal(oc, []string{"opencode/a", "opencode/b", "opencode/c", "opencode/d"}) {
		t.Errorf("opencode models = %q, want [opencode/a opencode/b opencode/c opencode/d]", oc)
	}

	// A cross-runtime lens with no per-lens model: an unnamed stage.
	if cl := runtimeModels(cfg, ls, "claude"); !slices.Equal(cl, []string{""}) {
		t.Errorf("claude models = %q, want [\"\"] (unnamed stage on cross-runtime lens)", cl)
	}
}

// newRunnerSet must hand each runtime the models its lenses will ask it for.
func TestRunnerSetHandsEachRuntimeItsModels(t *testing.T) {
	useRecordingPlatforms()
	cfg := store.Config{Runner: "fakerec", TriageModel: "fake/m1"}
	ls := []*lens.Lens{{Name: "a", ExtractModel: "fake/m2"}}
	t.Setenv("WITNESS_NO_DEFAULT_LENS", "1")
	st := openTestStore(t)
	rs, err := newRunnerSet(context.Background(), st, cfg, ls)
	if err != nil {
		t.Fatal(err)
	}
	defer rs.Close()
	minted, ok := modelRec.minted["fakerec"]
	if !ok {
		t.Fatal("NewRunner was not called")
	}
	if want := []string{"fake/m1", "fake/m2"}; !slices.Equal(minted.RunnerModels, want) {
		t.Errorf("RunnerModels = %q, want %q", minted.RunnerModels, want)
	}
}
