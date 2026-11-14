package commands

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/IngTian/witness/internal/lens"
	"github.com/IngTian/witness/internal/platform"
	"github.com/IngTian/witness/internal/store"
)

// openTestStore creates a fresh store in a temp dir for testing.
func openTestStore(t *testing.T) *store.Store {
	t.Helper()
	t.Setenv("WITNESS_HOME", t.TempDir())
	t.Setenv("WITNESS_NO_DEFAULT_LENS", "1")
	st, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	return st
}

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
			"implement the union (StartOpenCodeServerIn already accepts a model list) or keep the "+
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

// runtimeModels must return both stages of every lens routed to the given runtime, plus on the
// default runtime the cross-lens unified summary's review model. Empty models ("") are kept and
// the list is deduped.
func TestRuntimeModelsCoversEveryStageOnItsRuntime(t *testing.T) {
	cfg := store.Config{Runner: "opencode", TriageModel: "opencode/a"}
	ls := []*lens.Lens{
		{Name: "default"},
		{Name: "x", Runner: "claude"},
		{Name: "y", Runner: "opencode", ExtractModel: "opencode/b"},
	}

	// runtimeModels(cfg, ls, "opencode") == ["opencode/a","opencode/b"]
	// (y's review falls back to triage on the default runtime)
	oc := runtimeModels(cfg, ls, "opencode")
	want := map[string]bool{"opencode/a": true, "opencode/b": true}
	if len(oc) != len(want) {
		t.Errorf("opencode models len = %d, want %d: %v", len(oc), len(want), oc)
	}
	for _, m := range oc {
		if !want[m] {
			t.Errorf("opencode models contain unexpected %q", m)
		}
	}

	// runtimeModels(cfg, ls, "claude") == [""]
	// (a cross-runtime lens with no per-lens model: an unnamed stage)
	cl := runtimeModels(cfg, ls, "claude")
	if len(cl) != 1 || cl[0] != "" {
		t.Errorf("claude models = %v, want [\"\"] (unnamed stage on cross-runtime lens)", cl)
	}
}

// newRunnerSet must hand each runtime the models its lenses will ask it for.
func TestRunnerSetHandsEachRuntimeItsModels(t *testing.T) {
	// Register a fake that records the cfg from NewRunner
	var recorded *store.Config
	registerFakeModelsRecorder.Do(func() {
		platform.Register(&fakeModelsRecorderPlatform{rec: &recorded})
	})

	cfg := store.Config{Runner: "fakemodelsrec", TriageModel: "fake/m1"}
	ls := []*lens.Lens{{Name: "a", ExtractModel: "fake/m2"}}

	st := openTestStore(t)
	defer st.Close()
	ctx := context.Background()

	rs, err := newRunnerSet(ctx, st, cfg, ls)
	if err != nil {
		t.Fatal(err)
	}
	defer rs.Close()

	if recorded == nil {
		t.Fatal("NewRunner was not called")
	}

	got := recorded.RunnerModels
	want := runtimeModels(cfg, ls, "fakemodelsrec")
	if len(got) != len(want) {
		t.Errorf("RunnerModels len = %d, want %d: got %v, want %v", len(got), len(want), got, want)
	}
	// Build a set from want for comparison (order doesn't matter)
	wantSet := map[string]bool{}
	for _, m := range want {
		wantSet[m] = true
	}
	for _, m := range got {
		if !wantSet[m] {
			t.Errorf("RunnerModels contains unexpected %q; got %v, want %v", m, got, want)
		}
	}
}

var registerFakeModelsRecorder sync.Once

type fakeModelsRecorderPlatform struct {
	rec **store.Config
}

func (p *fakeModelsRecorderPlatform) Name() string          { return "fakemodelsrec" }
func (p *fakeModelsRecorderPlatform) SessionPrefix() string { return "fakemodelsrec:" }
func (p *fakeModelsRecorderPlatform) Capture(*store.Store, []byte, time.Time) (bool, error) {
	return false, nil
}
func (p *fakeModelsRecorderPlatform) NewRunner(cfg store.Config) platform.Runner {
	*p.rec = &cfg
	return fakeModelsRecorderRunner{}
}

type fakeModelsRecorderRunner struct{}

func (fakeModelsRecorderRunner) Open(context.Context) error { return nil }
func (fakeModelsRecorderRunner) Close() error               { return nil }
func (fakeModelsRecorderRunner) Run(context.Context, string, string, string) (string, error) {
	return "[]", nil
}
func (fakeModelsRecorderRunner) ValidateModels(context.Context, ...string) error { return nil }
func (fakeModelsRecorderRunner) InvocationHint() string                          { return "fakemodelsrec" }
