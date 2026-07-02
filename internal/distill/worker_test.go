package distill

import (
	"context"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"math"
	"strings"
	"testing"

	"github.com/IngTian/claude-witness/internal/lens"
	"github.com/IngTian/claude-witness/internal/store"
)

// fakeEmbedder returns a deterministic, well-separated unit vector per string:
// identical text -> identical vector (cosine 1, so dedup fires); different text
// -> near-orthogonal (low cosine, so distinct obs survive). No model needed.
type fakeEmbedder struct{}

func (fakeEmbedder) Embed(text string) ([]float32, error) {
	h := fnv.New64a()
	h.Write([]byte(text))
	seed := h.Sum64()
	v := make([]float32, 32)
	var ss float64
	for i := range v {
		seed = seed*6364136223846793005 + 1442695040888963407 // LCG
		f := float64(int64(seed>>11)) / float64(1<<53)
		v[i] = float32(f)
		ss += f * f
	}
	n := float32(math.Sqrt(ss))
	for i := range v {
		v[i] /= n
	}
	return v, nil
}

// fakeMiner records every transcript it was asked to mine and returns one
// observation whose text echoes the input, so tests can assert WHAT was mined.
// failsLeft > 0 makes the next N calls return an error (simulating claude -p
// failures) before succeeding — for the retry/dead-letter tests.
type fakeMiner struct {
	inputs    []string
	failsLeft int
}

func (m *fakeMiner) run(_ context.Context, _ string, _ string, input string) (string, error) {
	m.inputs = append(m.inputs, input)
	if m.failsLeft > 0 {
		m.failsLeft--
		return "", fmt.Errorf("simulated mine failure")
	}
	arr := []minedObs{{Dimension: "thinking", Observation: "obs-for:" + input, Evidence: "e", Poignancy: 3}}
	b, _ := json.Marshal(arr)
	return string(b), nil
}

func testWorker(s *store.Store, m *fakeMiner) *Worker {
	return &Worker{
		Store:    s,
		Embedder: fakeEmbedder{},
		Lenses:   []*lens.Lens{{Name: "default", Global: true, Extract: "mine", Dimensions: []string{"thinking"}}},
		Config:   store.Config{},
		Run:      m.run,
	}
}

func newStore(t *testing.T) *store.Store {
	t.Helper()
	t.Setenv("WITNESS_HOME", t.TempDir())
	s, err := store.Open()
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return s
}

func capture(t *testing.T, s *store.Store, session, role, text string) {
	t.Helper()
	if err := s.AppendRaw(store.RawRecord{Session: session, Seq: s.NextSeq(session), Role: role, Text: text}); err != nil {
		t.Fatalf("AppendRaw: %v", err)
	}
}

// Staged active observations are re-read by DrainStaged on every pass (it does
// not clear the file). Across a resume re-distill, the same active obs must not be
// appended twice — obsID dedup against L1 guards this.
func TestStagedActiveObsNotDuplicatedOnRedistill(t *testing.T) {
	s := newStore(t)
	m := &fakeMiner{}
	w := testWorker(s, m)
	ctx := context.Background()

	active := store.Observation{
		ID:          obsID("s", "default", "active-fact"),
		Session:     "s",
		Lens:        "default",
		Dimension:   "thinking",
		Observation: "active-fact",
	}
	if err := s.StageObservation(active); err != nil {
		t.Fatalf("StageObservation: %v", err)
	}

	capture(t, s, "s", "user", "alpha")
	capture(t, s, "s", "assistant", "reply-alpha")
	if err := w.Process(ctx, "s"); err != nil {
		t.Fatalf("first Process: %v", err)
	}

	capture(t, s, "s", "user", "beta")
	capture(t, s, "s", "assistant", "reply-beta")
	if err := w.Process(ctx, "s"); err != nil {
		t.Fatalf("second Process: %v", err)
	}

	obs, _ := s.ReadObservations("")
	n := 0
	for _, o := range obs {
		if o.ID == active.ID {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("active obs should appear exactly once after re-distill, got %d", n)
	}
}

// S2: after a successful pass the drained staged rows are removed, so they are
// not re-read and re-embedded on every subsequent pass (bounded staged growth).
func TestStagedClearedAfterSuccess(t *testing.T) {
	s := newStore(t)
	w := testWorker(s, &fakeMiner{})
	_ = s.StageObservation(store.Observation{
		ID: obsID("s", "default", "af"), Session: "s", Lens: "default", Observation: "af",
	})
	capture(t, s, "s", "user", "x")
	if err := w.Process(context.Background(), "s"); err != nil {
		t.Fatal(err)
	}
	drained, _, _ := s.DrainStaged("s")
	if len(drained) != 0 {
		t.Fatalf("staged rows should be cleared after a successful pass, got %d", len(drained))
	}
}

// On a FAILED pass the staged rows MUST survive so the active obs aren't lost.
func TestStagedRetainedAfterFailure(t *testing.T) {
	s := newStore(t)
	w := testWorker(s, &fakeMiner{failsLeft: 1})
	_ = s.StageObservation(store.Observation{
		ID: obsID("s", "default", "af"), Session: "s", Lens: "default", Observation: "af",
	})
	capture(t, s, "s", "user", "x")
	if err := w.Process(context.Background(), "s"); err != nil {
		t.Fatal(err)
	}
	drained, _, _ := s.DrainStaged("s")
	if len(drained) != 1 {
		t.Fatalf("staged rows must survive a failed pass for retry, got %d", len(drained))
	}
}

// A transient mine failure (claude -p hiccup) must NOT advance the watermark or
// write observations — the turns stay pending so the next run retries them.
func TestMineFailureRetriesWithoutLoss(t *testing.T) {
	s := newStore(t)
	m := &fakeMiner{failsLeft: 1}
	w := testWorker(s, m)

	capture(t, s, "s", "user", "alpha")
	capture(t, s, "s", "assistant", "reply")
	if err := w.Process(context.Background(), "s"); err != nil {
		t.Fatalf("Process: %v", err)
	}

	if obs, _ := s.ReadObservations(""); len(obs) != 0 {
		t.Fatalf("failed mine should write nothing, got %d obs", len(obs))
	}
	if got := s.DistilledCount("s"); got != 0 {
		t.Fatalf("watermark must NOT advance on failure, got %d", got)
	}
	if got := s.RetryCount("s"); got != 1 {
		t.Fatalf("retry count should be 1, got %d", got)
	}
}

// S1: a persistently-failing mine must NEVER advance the watermark or drop the
// delta. The raw turns stay pending so they self-heal when the failure clears.
// (Replaces the old data-losing dead-letter behavior.)
func TestMineFailureNeverDropsDelta(t *testing.T) {
	s := newStore(t)
	m := &fakeMiner{failsLeft: 999} // always fail
	w := testWorker(s, m)
	capture(t, s, "s", "user", "alpha")
	capture(t, s, "s", "assistant", "reply")

	for i := 0; i < 5; i++ {
		if err := w.Process(context.Background(), "s"); err != nil {
			t.Fatalf("Process attempt %d: %v", i, err)
		}
	}

	if got := s.DistilledCount("s"); got != 0 {
		t.Fatalf("watermark must NEVER advance while mining fails, got %d", got)
	}
	if obs, _ := s.ReadObservations(""); len(obs) != 0 {
		t.Fatalf("nothing should be written on persistent failure, got %d obs", len(obs))
	}
	if got := s.RetryCount("s"); got != 5 {
		t.Fatalf("retry count should track every failure, got %d", got)
	}
}

// After a failure the session is backed off: excluded from PendingSessions until
// its next_attempt passes, so the consumer doesn't hammer a failing session.
func TestFailureBacksOffFromQueue(t *testing.T) {
	s := newStore(t)
	m := &fakeMiner{failsLeft: 1}
	w := testWorker(s, m)
	capture(t, s, "s", "user", "alpha")
	capture(t, s, "s", "assistant", "reply")
	if err := w.Process(context.Background(), "s"); err != nil {
		t.Fatal(err)
	}
	p, _ := s.PendingSessions()
	for _, x := range p {
		if x == "s" {
			t.Fatalf("a backed-off session should be excluded from pending, got %v", p)
		}
	}
}

// A quiet session (model legitimately returns no parseable observations) is NOT
// a failure: the watermark advances with zero obs, no retry, no backoff.
func TestQuietSessionAdvancesWithoutRetry(t *testing.T) {
	s := newStore(t)
	w := testWorker(s, &fakeMiner{})
	w.Run = func(_ context.Context, _, _, _ string) (string, error) {
		return "Nothing notable happened this session.", nil // prose, no JSON array
	}
	capture(t, s, "s", "user", "alpha")
	capture(t, s, "s", "assistant", "reply")
	if err := w.Process(context.Background(), "s"); err != nil {
		t.Fatal(err)
	}
	if got := s.DistilledCount("s"); got != 2 {
		t.Fatalf("quiet session should advance watermark to 2, got %d", got)
	}
	if got := s.RetryCount("s"); got != 0 {
		t.Fatalf("quiet session is not a failure; retry should be 0, got %d", got)
	}
	if obs, _ := s.ReadObservations(""); len(obs) != 0 {
		t.Fatalf("quiet session yields no observations, got %d", len(obs))
	}
}

// A failure followed by a success must distill cleanly: obs written, watermark
// advanced, retry reset (no permanent loss from the transient failure).
func TestMineRecoversAfterFailure(t *testing.T) {
	s := newStore(t)
	m := &fakeMiner{failsLeft: 2}
	w := testWorker(s, m)
	capture(t, s, "s", "user", "alpha")
	capture(t, s, "s", "assistant", "reply")

	for i := 0; i < 3; i++ { // fail, fail, succeed
		if err := w.Process(context.Background(), "s"); err != nil {
			t.Fatalf("Process attempt %d: %v", i, err)
		}
	}

	if obs, _ := s.ReadObservations(""); len(obs) != 1 {
		t.Fatalf("recovered run should write the observation, got %d", len(obs))
	}
	if got := s.DistilledCount("s"); got != 2 {
		t.Fatalf("watermark should advance after success, got %d", got)
	}
	if got := s.RetryCount("s"); got != 0 {
		t.Fatalf("retry count should reset after success, got %d", got)
	}
}

// The core fix: a session resumed under the same id (new turns appended after an
// earlier distill) must have ONLY the new turns mined, with nothing lost.
func TestResumeDistillsOnlyNewTurnsWithoutLoss(t *testing.T) {
	s := newStore(t)
	m := &fakeMiner{}
	w := testWorker(s, m)
	ctx := context.Background()

	// First run of session "s": two turns.
	capture(t, s, "s", "user", "alpha-topic")
	capture(t, s, "s", "assistant", "reply-alpha")
	if err := w.Process(ctx, "s"); err != nil {
		t.Fatalf("first Process: %v", err)
	}

	// Resume: same session id, two new turns appended.
	capture(t, s, "s", "user", "beta-topic")
	capture(t, s, "s", "assistant", "reply-beta")
	if err := w.Process(ctx, "s"); err != nil {
		t.Fatalf("second Process: %v", err)
	}

	// The second mine must have seen the NEW turns only — not the old ones.
	if len(m.inputs) != 2 {
		t.Fatalf("expected 2 mine calls, got %d", len(m.inputs))
	}
	second := m.inputs[1]
	if !strings.Contains(second, "beta-topic") {
		t.Errorf("delta mine should include new turn; got:\n%s", second)
	}
	if strings.Contains(second, "alpha-topic") {
		t.Errorf("delta mine should NOT re-include already-distilled turns; got:\n%s", second)
	}

	// No loss: observations from BOTH runs are in L1, watermark at all 4 records.
	obs, _ := s.ReadObservations("")
	if len(obs) != 2 {
		t.Fatalf("expected 2 observations (one per run), got %d: %+v", len(obs), obs)
	}
	if got := s.DistilledCount("s"); got != 4 {
		t.Errorf("watermark should be 4 after distilling all turns, got %d", got)
	}
}
