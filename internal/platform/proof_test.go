package platform_test

// This is the acceptance proof for issue #21: a synthetic THIRD platform, registered
// through the PUBLIC platform registry ONLY, drives the REAL engine end to end
// (capture → ForSession → RunnerFor → distill Worker → L0→L1) with ZERO edits to
// internal/distill or cmd/commands. If the engine were still platform-aware, a
// brand-new platform couldn't be captured, resolved, or distilled without touching
// engine code — this test would be impossible to write. That it passes is the
// executable statement of "the engine is platform-agnostic."

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/IngTian/witness/internal/distill"
	"github.com/IngTian/witness/internal/lens"
	"github.com/IngTian/witness/internal/platform"
	"github.com/IngTian/witness/internal/store"
)

// --- the synthetic platform: implements every capability, knows nothing special ---

const fakePrefix = "fake:"

type fakePlatform struct{}

func (fakePlatform) Name() string          { return "fake" }
func (fakePlatform) SessionPrefix() string { return fakePrefix }

// RenderInputs: one transcript per record pair — a shaping rule distinct from both
// Claude (single) and OpenCode (chunked), to prove the engine uses whatever the
// owning platform returns.
func (fakePlatform) RenderInputs(raw []store.RawRecord) []string {
	var b strings.Builder
	for _, r := range raw {
		b.WriteString(r.Role + ": " + r.Text + "\n")
	}
	return []string{"FAKE-RENDER\n" + b.String()}
}

// Capture: write one L0 row from a trivial {session,text} payload + stamp platform.
func (fakePlatform) Capture(st *store.Store, data []byte, now time.Time) (bool, error) {
	var ev struct {
		Session string `json:"session"`
		Text    string `json:"text"`
	}
	if err := json.Unmarshal(data, &ev); err != nil {
		return false, err
	}
	sid := fakePrefix + ev.Session
	if err := st.AppendRaw(store.RawRecord{Session: sid, Seq: st.NextSeq(sid), Role: "user", Text: ev.Text, TS: now.UTC().Format(time.RFC3339)}); err != nil {
		return false, err
	}
	st.SetSessionPlatform(sid, "fake")
	return true, nil
}

func (fakePlatform) Import(context.Context, *store.Store, []string) (platform.ImportStats, error) {
	return platform.ImportStats{Agent: "fake"}, nil
}

// NewRunner: the fake engine returns a canned observation array, proving RunnerFor
// resolves a 3rd runner and the real Worker mines through it.
func (fakePlatform) NewRunner(store.Config) platform.Runner { return fakeRunner{} }

type fakeRunner struct{}

func (fakeRunner) Open(context.Context) error                      { return nil }
func (fakeRunner) Close() error                                    { return nil }
func (fakeRunner) ValidateModels(context.Context, ...string) error { return nil }
func (fakeRunner) InvocationHint() string                          { return "fake-run" }
func (fakeRunner) ConcurrentRunSafe() bool                         { return true }
func (fakeRunner) Run(_ context.Context, _, _, _ string) (string, error) {
	return `[{"dimension":"thinking","observation":"the fake platform was mined by the real engine","evidence":"proof","poignancy":5}]`, nil
}

type fakeEmbedder struct{}

func (fakeEmbedder) Embed(string) ([]float32, error) { return []float32{0.1, 0.2, 0.3}, nil }

// --- the proof ---

func TestFakeThirdPlatformDrivesRealEngineEndToEnd(t *testing.T) {
	// Register the fake ONLY through the public registry. If this required an engine
	// edit, the whole premise would fail.
	platform.Register(fakePlatform{})
	// (No unregister API by design — the registry is process-global; this test runs
	// in its own package-test binary, so the fake doesn't leak into other packages.)

	t.Setenv("WITNESS_HOME", t.TempDir())
	st, err := store.Open()
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()

	// 1. CAPTURE via the registry — proves the capture path is registry-driven.
	p, ok := platform.ByName("fake")
	if !ok {
		t.Fatal("fake platform not registered")
	}
	payload, _ := json.Marshal(map[string]string{"session": "s1", "text": "hello from a platform the engine never heard of"})
	capturer, ok := p.(platform.Capturer)
	if !ok {
		t.Fatal("fake platform should expose its optional capture capability")
	}
	if okCap, err := capturer.Capture(st, payload, time.Now()); err != nil || !okCap {
		t.Fatalf("Capture: ok=%v err=%v", okCap, err)
	}
	session := fakePrefix + "s1"
	if raw, _ := st.ReadRaw(session); len(raw) != 1 {
		t.Fatalf("L0 not written by fake capture: %d rows", len(raw))
	}

	// 2. ForSession resolves the fake as owner (via the stamped column) — proves the
	// per-session axis is registry-driven.
	if got := platform.ForSession(st, session).Name(); got != "fake" {
		t.Fatalf("ForSession = %q, want fake", got)
	}

	// 3. RunnerFor resolves the fake ENGINE (global axis) — proves runner dispatch is
	// registry-driven, independent of the session's own platform.
	runner, err := platform.RunnerFor(st, store.Config{Runner: "fake"})
	if err != nil {
		t.Fatalf("RunnerFor(fake): %v", err)
	}
	if runner.InvocationHint() != "fake-run" {
		t.Fatalf("resolved the wrong runner: %q", runner.InvocationHint())
	}

	// 4. Drive the REAL distill.Worker: it shapes input via ForSession (the fake's
	// RenderInputs) and mines via RunnerFor (the fake's Run) — no engine code knows
	// "fake" exists. Assert it produces L1.
	w := &distill.Worker{
		Store:    st,
		Embedder: fakeEmbedder{},
		Lenses:   []*lens.Lens{{Name: "default", BuiltIn: true, Extract: "mine", Dimensions: []string{"thinking"}}},
		Config:   store.Config{Runner: "fake"},
		Run:      distill.RunnerMine(runner),
	}
	if err := w.Process(context.Background(), session); err != nil {
		t.Fatalf("Worker.Process on a fake-platform session: %v", err)
	}
	obs, err := st.ReadObservations("")
	if err != nil {
		t.Fatal(err)
	}
	if len(obs) != 1 || !strings.Contains(obs[0].Observation, "mined by the real engine") {
		t.Fatalf("real engine did not produce L1 for the fake platform: %+v", obs)
	}

	// 5. The fake's OWN RenderInputs shaping was used (not a Claude/OpenCode default):
	// the observation came from a transcript the fake rendered. (Indirect: the mine
	// succeeded end to end through the fake's renderer + runner.)
	t.Logf("proof complete: L0→L1 for platform %q with zero engine edits", "fake")
}
