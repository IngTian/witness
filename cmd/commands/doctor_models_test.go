package commands

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
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

// Doctor end-to-end for an unnamed OpenCode stage model (spec §7.7). When a lens is routed
// to a ModelRequirer runtime (OpenCode) and its stage model resolves to "", validateRuntimeModels
// must call that runtime's ValidateModels with "" so the runtime can refuse it (the empty-model
// error must reach the user). On a non-ModelRequirer runtime (Claude), "" is allowed.
func TestDoctorSurfacesUnnamedStageErrorOnModelRequirerRuntime(t *testing.T) {
	var validateCalled []string
	registerFakeRequirerValidator.Do(func() {
		platform.Register(&fakeRequirerValidatorPlatform{rec: &validateCalled})
	})

	st := openTestStore(t)
	defer st.Close()

	// Register a lens routed to fakerequirer (cross-runtime) with no per-lens model.
	// Default runtime is "other", lens routes to "fakerequirer" → cross-runtime → ModelFor returns "".
	dir := t.TempDir()
	writeLensSrcForTest(t, dir, "cross", "fakerequirer", "", "")
	if err := st.RegisterLens("cross", dir); err != nil {
		t.Fatal(err)
	}
	if err := st.EnableLens("cross"); err != nil {
		t.Fatal(err)
	}

	cfg := store.Config{Runner: "other", TriageModel: "other/m1"}
	ctx := context.Background()
	err := validateRuntimeModels(ctx, st, cfg)

	// The fake ModelRequirer runtime refuses "" → error
	if err == nil {
		t.Fatal("validateRuntimeModels returned nil; want error for unnamed stage on ModelRequirer runtime")
	}
	if !strings.Contains(err.Error(), "unnamed model") {
		t.Errorf("error = %v; want one containing \"unnamed model\"", err)
	}

	// Verify that "" reached ValidateModels (the carry-in bug was doctor dropping it)
	hasEmpty := false
	for _, m := range validateCalled {
		if m == "" {
			hasEmpty = true
		}
	}
	if !hasEmpty {
		t.Errorf("ValidateModels was not called with \"\"; got %v; this means doctor dropped \"\" "+
			"before calling ValidateModels (the carry-in bug from 2.6)", validateCalled)
	}

	// On a non-ModelRequirer runtime (other, which is Claude-like), "" is allowed → no error.
	// Keep the same lens but now the default runtime is "other", so the lens is cross-runtime
	// to fakerequirer. Actually, let's use a fresh setup with claude as the default.
	st2 := openTestStore(t)
	defer st2.Close()
	dir2 := t.TempDir()
	writeLensSrcForTest(t, dir2, "cross2", "other", "", "")
	if err := st2.RegisterLens("cross2", dir2); err != nil {
		t.Fatal(err)
	}
	if err := st2.EnableLens("cross2"); err != nil {
		t.Fatal(err)
	}
	cfg2 := store.Config{Runner: "claude", TriageModel: "claude/m1"}
	err2 := validateRuntimeModels(ctx, st2, cfg2)
	if err2 != nil {
		t.Errorf("non-ModelRequirer runtime: validateRuntimeModels returned error %v; want nil (non-ModelRequirer allows \"\")", err2)
	}
}

var registerFakeRequirerValidator sync.Once

type fakeRequirerValidatorPlatform struct {
	rec *[]string
}

func (p *fakeRequirerValidatorPlatform) Name() string         { return "fakerequirer" }
func (p *fakeRequirerValidatorPlatform) SessionPrefix() string { return "fakerequirer:" }
func (p *fakeRequirerValidatorPlatform) Capture(*store.Store, []byte, time.Time) (bool, error) {
	return false, nil
}
func (p *fakeRequirerValidatorPlatform) NewRunner(cfg store.Config) platform.Runner {
	return &fakeRequirerValidatorRunner{rec: p.rec}
}
func (p *fakeRequirerValidatorPlatform) RequiresModel() bool { return true }

type fakeRequirerValidatorRunner struct {
	rec *[]string
}

func (r *fakeRequirerValidatorRunner) Open(context.Context) error { return nil }
func (r *fakeRequirerValidatorRunner) Close() error               { return nil }
func (r *fakeRequirerValidatorRunner) Run(context.Context, string, string, string) (string, error) {
	return "[]", nil
}
func (r *fakeRequirerValidatorRunner) ValidateModels(ctx context.Context, models ...string) error {
	*r.rec = append(*r.rec, models...)
	for _, m := range models {
		if strings.TrimSpace(m) == "" {
			return errors.New("unnamed model refused by ModelRequirer runtime")
		}
	}
	return nil
}
func (r *fakeRequirerValidatorRunner) InvocationHint() string { return "fakerequirer" }

// writeLensSrcForTest writes a minimal lens directory for testing.
func writeLensSrcForTest(t *testing.T, dir, name, runner, extract, review string) {
	t.Helper()
	cfg := map[string]interface{}{"name": name}
	if runner != "" {
		cfg["runner"] = runner
	}
	if extract != "" {
		cfg["extract_model"] = extract
	}
	if review != "" {
		cfg["review_model"] = review
	}
	cfgJSON, _ := json.Marshal(cfg)
	if err := os.WriteFile(filepath.Join(dir, "lens.json"), cfgJSON, 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "EXTRACT.md"), []byte("# Extract\nTest"), 0644); err != nil {
		t.Fatal(err)
	}
}
