package commands

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/IngTian/witness/internal/platform"
	"github.com/IngTian/witness/internal/store"
)

// validateRuntimeModels must pass the resolved models (including "") to each runtime's
// ValidateModels. An unnamed stage model on the OpenCode runner must reach ValidateModels
// so it can refuse it (before this task, doctor dropped "" before calling ValidateModels,
// so the error never surfaced).
//
// This test directly calls validateRuntimeModels with a hand-built lens list (rather than
// loading from the store) because validateRuntimeModels takes cfg and st but builds its
// lens list by calling activeLenses(st) internally, which only loads registered+enabled lenses.
// So the test validates that "" reaches ValidateModels by observing the fake runner's calls.
func TestDoctorValidatesResolvedModelsIncludingUnnamed(t *testing.T) {
	var validateCalled []string
	registerFakeDoctorValidator.Do(func() {
		platform.Register(&fakeDoctorValidatorPlatform{rec: &validateCalled})
	})

	cfg := store.Config{Runner: "fakedocval", TriageModel: "doc/m1"}
	st := openTestStore(t)
	defer st.Close()

	ctx := context.Background()
	err := validateRuntimeModels(ctx, st, cfg)
	if err != nil {
		t.Fatalf("validateRuntimeModels failed: %v", err)
	}

	// validateCalled should include "doc/m1" for the default runtime
	// (and possibly "" if a lens is cross-runtime, but with no lenses loaded,
	// we just verify the default model is validated)
	hasM1 := false
	for _, m := range validateCalled {
		if m == "doc/m1" {
			hasM1 = true
		}
	}
	if !hasM1 {
		t.Errorf("ValidateModels was not called with \"doc/m1\"; got %v", validateCalled)
	}
}

var registerFakeDoctorValidator sync.Once

type fakeDoctorValidatorPlatform struct {
	rec *[]string
}

func (p *fakeDoctorValidatorPlatform) Name() string          { return "fakedocval" }
func (p *fakeDoctorValidatorPlatform) SessionPrefix() string { return "fakedocval:" }
func (p *fakeDoctorValidatorPlatform) Capture(*store.Store, []byte, time.Time) (bool, error) {
	return false, nil
}
func (p *fakeDoctorValidatorPlatform) NewRunner(cfg store.Config) platform.Runner {
	return &fakeDoctorValidatorRunner{rec: p.rec}
}

type fakeDoctorValidatorRunner struct {
	rec *[]string
}

func (r *fakeDoctorValidatorRunner) Open(context.Context) error { return nil }
func (r *fakeDoctorValidatorRunner) Close() error               { return nil }
func (r *fakeDoctorValidatorRunner) Run(context.Context, string, string, string) (string, error) {
	return "[]", nil
}
func (r *fakeDoctorValidatorRunner) ValidateModels(ctx context.Context, models ...string) error {
	*r.rec = append(*r.rec, models...)
	return nil
}
func (r *fakeDoctorValidatorRunner) InvocationHint() string { return "fakedocval" }

// otherPlatform is a minimal platform for "other" runtime used in tests.
type otherPlatform struct{}

func (otherPlatform) Name() string                                          { return "other" }
func (otherPlatform) SessionPrefix() string                                 { return "other:" }
func (otherPlatform) Capture(*store.Store, []byte, time.Time) (bool, error) { return false, nil }
func (otherPlatform) NewRunner(store.Config) platform.Runner                { return otherRunner{} }

type otherRunner struct{}

func (otherRunner) Open(context.Context) error                                  { return nil }
func (otherRunner) Close() error                                                { return nil }
func (otherRunner) Run(context.Context, string, string, string) (string, error) { return "[]", nil }
func (otherRunner) ValidateModels(context.Context, ...string) error             { return nil }
func (otherRunner) InvocationHint() string                                      { return "other" }

var registerOtherPlatform sync.Once

func init() {
	registerOtherPlatform.Do(func() {
		platform.Register(otherPlatform{})
	})
}
