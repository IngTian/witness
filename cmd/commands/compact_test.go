package commands

import (
	"context"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/IngTian/witness/internal/platform"
	"github.com/IngTian/witness/internal/store"
)

// compactFakePlatform's runner answers compaction with one merge — a absorbs b and c — so the
// command runs plan → ask → apply for real, with no model.
type compactFakePlatform struct{ leakFakePlatform }

func (compactFakePlatform) Name() string                           { return "fakecompact" }
func (compactFakePlatform) SessionPrefix() string                  { return "fakecompact:" }
func (compactFakePlatform) NewRunner(store.Config) platform.Runner { return compactFakeRunner{} }

type compactFakeRunner struct{ leakFakeRunner }

func (compactFakeRunner) Close() error { return nil }
func (compactFakeRunner) Run(_ context.Context, _, system, input string) (string, error) {
	switch {
	case strings.Contains(input, "THE TRAIT YOU ARE WRITING"):
		return "one combined trait, written long enough to clear the guard that refuses a merge shorter than its largest input", nil
	case strings.Contains(system, "write its CLAIM"):
		return "[]", nil // the claim pass, after apply: claims are not under test here
	}
	return `[{"action":"merge","dimension":"thinking","key":"a","value":"merged","absorb":["b","c"],"confidence":0.8,"why":"same trait"}]`, nil
}

var registerCompactFake sync.Once

// compactFixture: an archive whose only lens holds three facets the fake runner will merge.
func compactFixture(t *testing.T) {
	t.Helper()
	registerCompactFake.Do(func() { platform.Register(compactFakePlatform{}) })
	t.Setenv(noDefaultLensEnv, "1")
	t.Setenv("WITNESS_HOME", t.TempDir())
	st, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.RegisterLens("math", writeLensDir(t, "math", "mine math", "fold math")); err != nil {
		t.Fatal(err)
	}
	if err := st.EnableLens("math"); err != nil {
		t.Fatal(err)
	}
	if err := st.SetConfigString("runner", "fakecompact"); err != nil {
		t.Fatal(err)
	}
	var fs []store.Facet
	for _, k := range []string{"a", "b", "c"} {
		fs = append(fs, store.Facet{Lens: "math", Dimension: "thinking", Key: k, LastSeen: "2026-10-01T00:00:00Z",
			Versions: []store.FacetVersion{{Value: "value " + k, Confidence: 0.7, RecordedAt: "2026-10-01T00:00:00Z",
				ValidFrom: "2026-10-01T00:00:00Z", BecauseOf: []string{"obs-" + k}}}})
	}
	if err := st.WriteFacets(fs); err != nil {
		t.Fatal(err)
	}
}

func compactCounts(t *testing.T) (active, archived int) {
	t.Helper()
	st, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	return st.LensFacetCounts("math")
}

// compact shows the plan and asks. Declining writes nothing and exits 0, as the dry run always
// did; --yes, and its old name --apply, apply without asking.
func TestCompactShowsThePlanThenAsks(t *testing.T) {
	compactFixture(t)
	var asked bool
	var err error
	out := captureStdout(t, func() { err = cmdCompact(false, "", answer(false, &asked)) })
	if err != nil {
		t.Fatalf("declining is not an error: %v", err)
	}
	if !asked || !strings.Contains(out, "merge →") || !strings.Contains(out, "not applied") {
		t.Errorf("the plan must be shown, then asked about (asked=%v):\n%s", asked, out)
	}
	if a, ar := compactCounts(t); a != 3 || ar != 0 {
		t.Fatalf("declining applied the plan: %d active, %d archived", a, ar)
	}

	_ = captureStdout(t, func() { err = cmdCompact(false, "", answer(true, nil)) })
	if a, ar := compactCounts(t); err != nil || a != 1 || ar != 2 {
		t.Fatalf("y must apply the plan shown: %d active, %d archived, err %v", a, ar, err)
	}
}

// Through the real command, so the flag WIRING is what is tested: stdin is empty, so had either
// flag failed to reach cmdCompact, the prompt would read EOF and decline.
func TestCompactYesAndApplySkipThePrompt(t *testing.T) {
	for _, flag := range []string{"--yes", "--apply"} {
		t.Run(flag, func(t *testing.T) {
			compactFixture(t)
			r, w, err := os.Pipe()
			if err != nil {
				t.Fatal(err)
			}
			w.Close()
			old := os.Stdin
			os.Stdin = r
			defer func() { os.Stdin = old; r.Close() }()
			root := newRootCmd()
			root.SetArgs([]string{"compact", flag})
			out := captureStdout(t, func() {
				if err := root.Execute(); err != nil {
					t.Fatal(err)
				}
			})
			if a, ar := compactCounts(t); strings.Contains(out, "Apply this plan?") || a != 1 || ar != 2 {
				t.Errorf("%s must apply without asking: %d active, %d archived:\n%s", flag, a, ar, out)
			}
		})
	}
}
