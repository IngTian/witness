package commands

import (
	"bytes"
	"context"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/IngTian/witness/internal/platform"
	"github.com/IngTian/witness/internal/store"
)

// registerFakeModel guards registration of fakemodel platform (a second Register panics).
var registerFakeModel sync.Once

// fakeModelPlatform is a minimal registered platform that implements ModelRequirer and
// returns true, used to test the model-asking flow when wiring a runner that needs one.
type fakeModelPlatform struct{}

func (fakeModelPlatform) Name() string                                          { return "fakemodel" }
func (fakeModelPlatform) SessionPrefix() string                                 { return "fakemodel:" }
func (fakeModelPlatform) Capture(*store.Store, []byte, time.Time) (bool, error) { return false, nil }
func (fakeModelPlatform) NewRunner(store.Config) platform.Runner                { return fakeModelRunner{} }
func (fakeModelPlatform) RequiresModel() bool                                   { return true }

type fakeModelRunner struct{}

func (fakeModelRunner) Open(context.Context) error                                  { return nil }
func (fakeModelRunner) Close() error                                                { return nil }
func (fakeModelRunner) Run(context.Context, string, string, string) (string, error) { return "[]", nil }
func (fakeModelRunner) ValidateModels(context.Context, ...string) error             { return nil }
func (fakeModelRunner) InvocationHint() string                                      { return "fakemodel" }

// wireOutput runs `witness wire target` and returns what it printed, failing the test on an error.
func wireOutput(t *testing.T, target string) string {
	t.Helper()
	var err error
	out := captureStdout(t, func() { err = cmdInstall([]string{target}) })
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// hasLine reports whether one line of out contains every part, so a row is matched whole and not
// by words scattered over the screen.
func hasLine(out string, parts ...string) bool {
	for _, line := range strings.Split(out, "\n") {
		all := true
		for _, p := range parts {
			all = all && strings.Contains(line, p)
		}
		if all {
			return true
		}
	}
	return false
}

// TestWireBindsTheRunnerWhenNoneIsChosenYet verifies that on a fresh archive (no runner
// bound), wire binds the wired target as the default and reports "the default".
func TestWireBindsTheRunnerWhenNoneIsChosenYet(t *testing.T) {
	registerLeakFake.Do(func() { platform.Register(leakFakePlatform{}) })
	t.Setenv("WITNESS_HOME", t.TempDir())
	t.Setenv("WITNESS_NO_DEFAULT_LENS", "1")

	var called bool
	registerFakeCaptureOnlyEditor(t, "fakeleak", &called)

	out := wireOutput(t, "fakeleak")
	if !called {
		t.Fatal("installer was not called")
	}
	if !hasLine(out, "runner", "fakeleak", "the default") {
		t.Errorf("wire output has no `runner fakeleak (the default)` row:\n%s", out)
	}

	st, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if !st.RunnerBound() {
		t.Fatalf("RunnerBound() = false, want true")
	}
	if got := st.ResolvedConfig().Runner; got != "fakeleak" {
		t.Errorf("runner = %q, want %q", got, "fakeleak")
	}
}

// TestWireNeverSwitchesABoundRunnerWithoutATerminal verifies that when a runner is
// already bound and wireAnswers returns nil (no terminal), wire leaves it unchanged
// and prints the hint "witness config set runner fakeleak".
func TestWireNeverSwitchesABoundRunnerWithoutATerminal(t *testing.T) {
	registerLeakFake.Do(func() { platform.Register(leakFakePlatform{}) })
	t.Setenv("WITNESS_HOME", t.TempDir())
	t.Setenv("WITNESS_NO_DEFAULT_LENS", "1")

	// Bind claude as the runner first.
	st, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetRunner("claude"); err != nil {
		t.Fatal(err)
	}
	st.Close()

	var called bool
	registerFakeCaptureOnlyEditor(t, "fakeleak", &called)

	out := wireOutput(t, "fakeleak")
	// Piped, this hint is the only guidance there is (Global Constraints)
	if !hasLine(out, "runner", "unchanged (claude)") || !hasLine(out, "witness config set runner fakeleak") {
		t.Errorf("wire output without a terminal lacks the unchanged row or the `witness config set runner fakeleak` hint:\n%s", out)
	}
	if strings.Contains(out, "[y/N]") {
		t.Errorf("wire asked without a terminal:\n%s", out)
	}

	st, err = store.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if got := st.ResolvedConfig().Runner; got != "claude" {
		t.Errorf("runner = %q, want %q (unchanged)", got, "claude")
	}
}

// TestWireAsksBeforeSwitchingABoundRunner verifies that with a terminal input,
// wire asks whether to switch and respects the answer: "y\n" switches, "\n" and
// "n\n" keep the existing runner, and the prompt contains "[y/N]".
func TestWireAsksBeforeSwitchingABoundRunner(t *testing.T) {
	registerLeakFake.Do(func() { platform.Register(leakFakePlatform{}) })

	for _, tc := range []struct {
		name     string
		answer   string
		wantNext string
	}{
		{"yes", "y\n", "fakeleak"},
		{"default_no", "\n", "claude"},
		{"explicit_no", "n\n", "claude"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("WITNESS_HOME", t.TempDir())
			t.Setenv("WITNESS_NO_DEFAULT_LENS", "1")

			// Bind claude first.
			st, err := store.Open()
			if err != nil {
				t.Fatal(err)
			}
			if err := st.SetRunner("claude"); err != nil {
				t.Fatal(err)
			}
			st.Close()

			var called bool
			registerFakeCaptureOnlyEditor(t, "fakeleak", &called)

			// Stub wireAnswers to return the test answer.
			orig := wireAnswers
			wireAnswers = func() io.Reader { return bytes.NewBufferString(tc.answer) }
			t.Cleanup(func() { wireAnswers = orig })

			out := wireOutput(t, "fakeleak")
			if !strings.Contains(out, "Make fakeleak the default runner instead of claude? [y/N]") {
				t.Errorf("wire did not ask with a [y/N] prompt:\n%s", out)
			}
			if tc.wantNext == "fakeleak" {
				if !hasLine(out, "runner", "fakeleak", "the default") {
					t.Errorf("a yes did not report `runner fakeleak (the default)`:\n%s", out)
				}
			} else if !hasLine(out, "witness config set runner fakeleak") {
				t.Errorf("a no did not print the `witness config set runner fakeleak` hint:\n%s", out)
			}

			st, err = store.Open()
			if err != nil {
				t.Fatal(err)
			}
			defer st.Close()
			if got := st.ResolvedConfig().Runner; got != tc.wantNext {
				t.Errorf("runner = %q, want %q", got, tc.wantNext)
			}
		})
	}
}

// TestWireAsksForModelsWhenTheNewDefaultNeedsOne verifies that when wiring a runner
// that implements ModelRequirer with RequiresModel() true and the mine model is empty,
// wire asks for models at a terminal, writes them, and on invalid input or no terminal
// prints the hint "witness config set mine_model <provider/model>".
func TestWireAsksForModelsWhenTheNewDefaultNeedsOne(t *testing.T) {
	registerFakeModel.Do(func() { platform.Register(fakeModelPlatform{}) })

	for _, tc := range []struct {
		name        string
		input       string
		wantMine    string
		wantReview  string
		wantNoWrite bool
	}{
		{"mine_only", "prov/m1\n\n", "prov/m1", "", false},
		{"both", "prov/m1\nprov/m2\n", "prov/m1", "prov/m2", false},
		{"invalid", "nonsense\n", "", "", true},
		// A bad review model refuses both: a half-set pair would hide the refusal behind a working mine
		{"invalid_review", "prov/m1\nnonsense\n", "", "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("WITNESS_HOME", t.TempDir())
			t.Setenv("WITNESS_NO_DEFAULT_LENS", "1")

			var called bool
			registerFakeCaptureOnlyEditor(t, "fakemodel", &called)

			// Stub wireAnswers to return the test input.
			orig := wireAnswers
			wireAnswers = func() io.Reader { return bytes.NewBufferString(tc.input) }
			t.Cleanup(func() { wireAnswers = orig })

			out := wireOutput(t, "fakemodel")
			if got := hasLine(out, "witness config set mine_model <provider/model>"); got != tc.wantNoWrite {
				t.Errorf("mine_model hint printed = %v, want %v:\n%s", got, tc.wantNoWrite, out)
			}

			st, err := store.Open()
			if err != nil {
				t.Fatal(err)
			}
			defer st.Close()
			cfg := st.ResolvedConfig()
			if tc.wantNoWrite {
				if cfg.TriageModel != "" || cfg.DistillModel != "" {
					t.Errorf("TriageModel = %q, DistillModel = %q, want both empty (invalid input, nothing written)", cfg.TriageModel, cfg.DistillModel)
				}
			} else {
				if cfg.TriageModel != tc.wantMine {
					t.Errorf("TriageModel = %q, want %q", cfg.TriageModel, tc.wantMine)
				}
				if cfg.DistillModel != tc.wantReview {
					t.Errorf("DistillModel = %q, want %q", cfg.DistillModel, tc.wantReview)
				}
			}
		})
	}

	// nil wireAnswers path (no terminal).
	t.Run("no_terminal", func(t *testing.T) {
		t.Setenv("WITNESS_HOME", t.TempDir())
		t.Setenv("WITNESS_NO_DEFAULT_LENS", "1")

		var called bool
		registerFakeCaptureOnlyEditor(t, "fakemodel", &called)

		out := wireOutput(t, "fakemodel")
		if !hasLine(out, "witness config set mine_model <provider/model>") {
			t.Errorf("wire without a terminal did not print the mine_model hint:\n%s", out)
		}
		if strings.Contains(out, "mine model (") {
			t.Errorf("wire asked for a model without a terminal:\n%s", out)
		}

		st, err := store.Open()
		if err != nil {
			t.Fatal(err)
		}
		defer st.Close()
		if st.ResolvedConfig().TriageModel != "" {
			t.Errorf("TriageModel = %q, want empty (no terminal, nothing written)", st.ResolvedConfig().TriageModel)
		}
	})

	// When a mine model is already set, no question is asked.
	t.Run("model_already_set", func(t *testing.T) {
		t.Setenv("WITNESS_HOME", t.TempDir())
		t.Setenv("WITNESS_NO_DEFAULT_LENS", "1")

		st, err := store.Open()
		if err != nil {
			t.Fatal(err)
		}
		if err := st.SetConfigString("triage_model", "existing/model"); err != nil {
			t.Fatal(err)
		}
		st.Close()

		var called bool
		registerFakeCaptureOnlyEditor(t, "fakemodel", &called)

		if err := cmdInstall([]string{"fakemodel"}); err != nil {
			t.Fatal(err)
		}

		st, err = store.Open()
		if err != nil {
			t.Fatal(err)
		}
		defer st.Close()
		if got := st.ResolvedConfig().TriageModel; got != "existing/model" {
			t.Errorf("TriageModel = %q, want %q (unchanged)", got, "existing/model")
		}
	})
}

// TestWireHelpStatesTheRunnerRule verifies that the wire Long text no longer says
// "never prompts" and still mentions "witness config set runner" and runnerChoice().
func TestWireHelpStatesTheRunnerRule(t *testing.T) {
	c := newWireCmd()
	if strings.Contains(c.Long, "never prompts") {
		t.Errorf("wire Long still contains %q, which is no longer true", "never prompts")
	}
	if !strings.Contains(c.Long, "witness config set runner ") {
		t.Errorf("wire Long missing %q", "witness config set runner ")
	}
	// runnerChoice() is called in the Long text, so its result should appear.
	choice := runnerChoice()
	if choice == "" {
		t.Fatal("runnerChoice() returned empty")
	}
	if !strings.Contains(c.Long, choice) {
		t.Errorf("wire Long missing runnerChoice() result %q", choice)
	}
}
