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

// mineModelHint is the fix wire prints whenever it leaves the mine model unset, spelled out here
// rather than shared with install.go so a change to the user-facing text fails a test.
const mineModelHint = "witness config set mine_model <provider/model>"

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

// wireWith runs `witness wire target` on a fresh archive with bound as its runner and preset as its
// mine model ("" sets neither), answering the prompts with answers ("": no terminal, so nothing is
// asked). It returns what wire printed and the config it left, which always has a runner bound.
func wireWith(t *testing.T, target, bound, preset, answers string) (string, store.Config) {
	t.Helper()
	t.Setenv("WITNESS_HOME", t.TempDir())
	t.Setenv("WITNESS_NO_DEFAULT_LENS", "1")
	if bound != "" || preset != "" {
		st, err := store.Open()
		if err != nil {
			t.Fatal(err)
		}
		if bound != "" {
			err = st.SetRunner(bound)
		}
		if err == nil && preset != "" {
			err = st.SetConfigString("triage_model", preset)
		}
		st.Close()
		if err != nil {
			t.Fatal(err)
		}
	}
	var called bool
	registerFakeCaptureOnlyEditor(t, target, &called)
	if answers != "" {
		orig := wireAnswers
		wireAnswers = func() io.Reader { return bytes.NewBufferString(answers) }
		t.Cleanup(func() { wireAnswers = orig })
	}
	var err error
	out := captureStdout(t, func() { err = cmdInstall([]string{target}) })
	if err != nil {
		t.Fatal(err)
	}
	if !called {
		t.Fatal("installer was not called")
	}
	st, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if !st.RunnerBound() {
		t.Fatalf("RunnerBound() = false, want true")
	}
	return out, st.ResolvedConfig()
}

// wire binds the wired agent as the default runner when none is bound. With another one bound it
// asks only at a terminal, defaulting to no, and otherwise leaves it and prints the command that
// switches it: piped, that hint is the only guidance there is (Global Constraints).
func TestWireChoosesTheDefaultRunner(t *testing.T) {
	registerLeakFake.Do(func() { platform.Register(leakFakePlatform{}) })
	const prompt = "Make fakeleak the default runner instead of claude? [y/N]"
	for _, tc := range []struct {
		name, bound, answers, want string
	}{
		{"none bound", "", "", "fakeleak"},
		{"bound, no terminal", "claude", "", "claude"},
		{"bound, yes", "claude", "y\n", "fakeleak"},
		{"bound, default no", "claude", "\n", "claude"},
		{"bound, explicit no", "claude", "n\n", "claude"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, cfg := wireWith(t, "fakeleak", tc.bound, "", tc.answers)
			if cfg.Runner != tc.want {
				t.Errorf("runner = %q, want %q", cfg.Runner, tc.want)
			}
			if asked := strings.Contains(out, "[y/N]"); asked != (tc.bound != "" && tc.answers != "") || asked && !strings.Contains(out, prompt) {
				t.Errorf("asked = %v; want the %q prompt only at a terminal with another runner bound:\n%s", asked, prompt, out)
			}
			// The whole row: the prompt line also holds "runner", "fakeleak" and "the default"
			if tc.want == "fakeleak" && !strings.Contains(out, resultRow(okGlyph(), "runner", "fakeleak", "the default")+"\n") {
				t.Errorf("no `runner fakeleak (the default)` row:\n%s", out)
			}
			if tc.want == "claude" && (!hasLine(out, "runner", "unchanged (claude)") || !hasLine(out, "witness config set runner fakeleak")) {
				t.Errorf("no unchanged row or `witness config set runner fakeleak` hint:\n%s", out)
			}
		})
	}
}

// When the new default runner needs a model and no mine model is set, wire asks at a terminal and
// writes the answers; without a terminal it asks nothing. An answer that is not provider/model
// refuses both, since a half-set pair would hide the refusal behind a working mine. Whenever the
// mine model stays unset the hint names the fix. A mine model already set is never asked about.
func TestWireAsksForModelsWhenTheNewDefaultNeedsOne(t *testing.T) {
	registerFakeModel.Do(func() { platform.Register(fakeModelPlatform{}) })
	for _, tc := range []struct {
		name, preset, answers string
		mine, review          string
		hint                  bool
	}{
		{"mine_only", "", "prov/m1\n\n", "prov/m1", "", false},
		{"both", "", "prov/m1\nprov/m2\n", "prov/m1", "prov/m2", false},
		{"invalid", "", "nonsense\n", "", "", true},
		{"invalid_review", "", "prov/m1\nnonsense\n", "", "", true},
		{"no_terminal", "", "", "", "", true},
		{"model_already_set", "existing/model", "", "existing/model", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, cfg := wireWith(t, "fakemodel", "", tc.preset, tc.answers)
			if got := hasLine(out, mineModelHint); got != tc.hint {
				t.Errorf("mine_model hint printed = %v, want %v:\n%s", got, tc.hint, out)
			}
			if asked := strings.Contains(out, "mine model ("); asked != (tc.answers != "") {
				t.Errorf("asked for a model = %v, want %v:\n%s", asked, tc.answers != "", out)
			}
			if cfg.TriageModel != tc.mine || cfg.DistillModel != tc.review {
				t.Errorf("TriageModel = %q, DistillModel = %q, want %q, %q", cfg.TriageModel, cfg.DistillModel, tc.mine, tc.review)
			}
		})
	}
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
