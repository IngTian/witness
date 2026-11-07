package commands

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/IngTian/witness/internal/lens"
	"github.com/IngTian/witness/internal/platform"
	"github.com/IngTian/witness/internal/store"
)

// The prompt-injection fence is applied once, in distill.RunnerMine, and no runner applies it
// itself. So a call site that hands the engine runner.Run directly sends the corpus to the
// model unfenced and with no CorpusNotice, and every runner test still passes. These pin the
// two production call sites, the drain's runnerSet and `lens try`, with a recording runner.

type fenceCall struct{ prompt, input string }

var (
	fenceMu    sync.Mutex
	fenceCalls []fenceCall
	fenceOnce  sync.Once
)

type fenceRecordingRunner struct{}

func (fenceRecordingRunner) Open(context.Context) error { return nil }
func (fenceRecordingRunner) Close() error               { return nil }
func (fenceRecordingRunner) Run(_ context.Context, _, prompt, input string) (string, error) {
	fenceMu.Lock()
	defer fenceMu.Unlock()
	fenceCalls = append(fenceCalls, fenceCall{prompt, input})
	return "[]", nil
}
func (fenceRecordingRunner) ValidateModels(context.Context, ...string) error { return nil }
func (fenceRecordingRunner) InvocationHint() string                          { return "fakefence" }

type fenceFakePlatform struct{}

func (fenceFakePlatform) Name() string                           { return "fakefence" }
func (fenceFakePlatform) SessionPrefix() string                  { return "fakefence:" }
func (fenceFakePlatform) NewRunner(store.Config) platform.Runner { return fenceRecordingRunner{} }

func resetFenceCalls(t *testing.T) {
	fenceMu.Lock()
	fenceCalls = nil
	fenceMu.Unlock()
	t.Cleanup(func() { fenceMu.Lock(); fenceCalls = nil; fenceMu.Unlock() })
}

// assertFenced checks every recorded call carried CorpusNotice and exactly one fence.
func assertFenced(t *testing.T, wantInner string) {
	t.Helper()
	fenceMu.Lock()
	defer fenceMu.Unlock()
	if len(fenceCalls) == 0 {
		t.Fatal("the recording runner was never called; the test proves nothing")
	}
	open, closing, _ := strings.Cut(platform.WrapCorpus("\x00"), "\x00")
	for _, c := range fenceCalls {
		if !strings.HasSuffix(c.prompt, "\n\n"+platform.CorpusNotice) {
			t.Errorf("system prompt reached the runner without CorpusNotice: %q", c.prompt)
		}
		inner, ok := strings.CutPrefix(c.input, open)
		if inner, ok = strings.CutSuffix(inner, closing); !ok || strings.Contains(inner, open) {
			t.Errorf("corpus reached the runner not fenced exactly once: %q", c.input)
		}
		if !strings.Contains(inner, wantInner) {
			t.Errorf("fenced corpus lost its content %q: %q", wantInner, c.input)
		}
	}
}

func TestRunnerSetFencesEveryRun(t *testing.T) {
	t.Setenv("WITNESS_HOME", t.TempDir())
	st, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	fenceOnce.Do(func() { platform.Register(fenceFakePlatform{}) })
	resetFenceCalls(t)

	ln := &lens.Lens{Name: "default"}
	rs, err := newRunnerSet(context.Background(), st, store.Config{Runner: "fakefence"}, []*lens.Lens{ln})
	if err != nil {
		t.Fatalf("newRunnerSet: %v", err)
	}
	defer rs.Close()
	if _, err := rs.RunFor(ln)(context.Background(), "m", "EXTRACT", "the corpus"); err != nil {
		t.Fatal(err)
	}
	assertFenced(t, "the corpus")
}

func TestLensTryFencesEveryRun(t *testing.T) {
	seedTryStore(t, store.RunnerClaude)
	resetFenceCalls(t)
	previous := runnerForName
	runnerForName = func(string, store.Config) (platform.Runner, error) {
		return fenceRecordingRunner{}, nil
	}
	t.Cleanup(func() { runnerForName = previous })

	if err := cmdLensTry(validLensDir(t), lensTryOpts{nSessions: 1}); err != nil {
		t.Fatalf("lens try: %v", err)
	}
	assertFenced(t, "hello") // seedTryStore's one raw turn
}
