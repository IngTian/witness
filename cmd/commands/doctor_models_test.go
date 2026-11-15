package commands

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/IngTian/witness/internal/platform"
	"github.com/IngTian/witness/internal/store"
)

// recordingPlatform is the one fake runtime the doctor and runnerSet model tests register. Its
// runners record the cfg they are minted with and the models ValidateModels is handed, and with
// requiresModel ValidateModels refuses an unnamed ("") model, as OpenCode's does. A platform
// registers once per process, so the record is package-level and each test resets it
// (useRecordingPlatforms; the freeFakeRuns pattern): a recorder captured by the sync.Once
// closure belonged to the first run, and at -count=2 every later run recorded into it.
type recordingPlatform struct {
	name          string
	requiresModel bool
}

var (
	registerRecordingPlatforms sync.Once
	modelRec                   struct {
		minted    map[string]store.Config // runtime → the cfg its runner was minted with
		validated map[string][]string     // runtime → the models its ValidateModels was handed
	}
)

// useRecordingPlatforms registers fakerequirer (refuses "") and fakerec (accepts anything) and
// clears the record.
func useRecordingPlatforms() {
	registerRecordingPlatforms.Do(func() {
		platform.Register(recordingPlatform{name: "fakerequirer", requiresModel: true})
		platform.Register(recordingPlatform{name: "fakerec"})
	})
	modelRec.minted, modelRec.validated = map[string]store.Config{}, map[string][]string{}
}

func (p recordingPlatform) Name() string          { return p.name }
func (p recordingPlatform) SessionPrefix() string { return p.name + ":" }
func (p recordingPlatform) NewRunner(cfg store.Config) platform.Runner {
	modelRec.minted[p.name] = cfg
	return recordingRunner{p}
}

type recordingRunner struct{ p recordingPlatform }

func (recordingRunner) Open(context.Context) error { return nil }
func (recordingRunner) Close() error               { return nil }
func (recordingRunner) Run(context.Context, string, string, string) (string, error) {
	return "[]", nil
}
func (r recordingRunner) ValidateModels(_ context.Context, models ...string) error {
	modelRec.validated[r.p.name] = append(modelRec.validated[r.p.name], models...)
	if r.p.requiresModel && slices.Contains(models, "") {
		return errors.New("unnamed model refused by ModelRequirer runtime")
	}
	return nil
}
func (r recordingRunner) InvocationHint() string { return r.p.name }

// registerLens registers and enables a lens routed to runner, with no per-lens model.
func registerLens(t *testing.T, st *store.Store, name, runner string) {
	t.Helper()
	if err := st.RegisterLens(name, writeLensDir(t, name, "mine", "", store.LensKeyRunner, runner)); err != nil {
		t.Fatal(err)
	}
	if err := st.EnableLens(name); err != nil {
		t.Fatal(err)
	}
}

// Doctor end-to-end for an unnamed OpenCode stage model (spec §7.7). validateRuntimeModels hands
// each runtime the models its lenses resolve to, "" included: a lens routed to another runtime
// with no per-lens model resolves to "" there, and a runtime that requires a model must get the
// chance to refuse it (before 2.7, doctor dropped "" before calling ValidateModels, so the error
// never surfaced). A runtime that does not require one accepts it.
func TestDoctorSurfacesUnnamedStageErrorOnModelRequirerRuntime(t *testing.T) {
	useRecordingPlatforms()
	st := openTestStore(t)
	defer st.Close()
	registerLens(t, st, "cross", "fakerequirer")
	err := validateRuntimeModels(context.Background(), st, store.Config{Runner: "fakerec", TriageModel: "fakerec/m1"})
	if err == nil || !strings.Contains(err.Error(), "unnamed model") {
		t.Fatalf("validateRuntimeModels = %v, want the requiring runtime's unnamed-model refusal", err)
	}
	if got := modelRec.validated["fakerequirer"]; !slices.Equal(got, []string{""}) {
		t.Errorf("fakerequirer validated %q, want [\"\"]: doctor dropped the unnamed stage", got)
	}

	useRecordingPlatforms()
	st2 := openTestStore(t)
	defer st2.Close()
	registerLens(t, st2, "cross2", "fakerec")
	if err := validateRuntimeModels(context.Background(), st2, store.Config{Runner: "claude", TriageModel: "claude/m1"}); err != nil {
		t.Errorf("validateRuntimeModels = %v, want nil: a runtime that requires no model accepts \"\"", err)
	}
	if got := modelRec.validated["fakerec"]; !slices.Equal(got, []string{""}) {
		t.Errorf("fakerec validated %q, want [\"\"]", got)
	}
}
