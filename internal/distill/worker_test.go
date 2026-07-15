package distill

import (
	"context"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"math"
	"strings"
	"testing"

	"github.com/IngTian/witness/internal/lens"
	_ "github.com/IngTian/witness/internal/platform/claude" // register the default platform for ForSession
	opencodeplatform "github.com/IngTian/witness/internal/platform/opencode"
	"github.com/IngTian/witness/internal/store"
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

func TestOpenCodeSessionsAreChunked(t *testing.T) {
	s := newStore(t)
	m := &fakeMiner{}
	w := testWorker(s, m)
	defer opencodeplatform.SetChunkMaxCharsForTest(18)()

	capture(t, s, "opencode:s", "user", "alpha alpha alpha")
	capture(t, s, "opencode:s", "assistant", "beta beta beta")
	capture(t, s, "opencode:s", "user", "gamma gamma gamma")
	if err := w.Process(context.Background(), "opencode:s"); err != nil {
		t.Fatal(err)
	}
	if len(m.inputs) <= 1 {
		t.Fatalf("OpenCode long transcript should be chunked, got %d input(s): %#v", len(m.inputs), m.inputs)
	}
}

func TestNonOpenCodeSessionsUseSingleLegacyTranscript(t *testing.T) {
	s := newStore(t)
	m := &fakeMiner{}
	w := testWorker(s, m)
	defer opencodeplatform.SetChunkMaxCharsForTest(18)()

	capture(t, s, "claude:s", "user", "alpha alpha alpha")
	capture(t, s, "claude:s", "assistant", "beta beta beta")
	capture(t, s, "claude:s", "user", "gamma gamma gamma")
	if err := w.Process(context.Background(), "claude:s"); err != nil {
		t.Fatal(err)
	}
	if len(m.inputs) != 1 {
		t.Fatalf("non-OpenCode transcript should preserve legacy single input, got %d", len(m.inputs))
	}
}

// replaceRaw simulates an OpenCode replace-import landing mid-mine: it DELETEs the
// session's raw + progress (the reset ApplyRawImport(replace=true) does) and
// re-INSERTs a fresh generation of turns. Because raw.id is AUTOINCREMENT, the new
// rows get strictly higher ids than anything a prior mine read.
func replaceRaw(t *testing.T, s *store.Store, session string, turns []string) {
	t.Helper()
	meta := store.SessionMeta{Session: session}
	recs := make([]store.RawRecord, len(turns))
	for i, txt := range turns {
		recs[i] = store.RawRecord{Session: session, Seq: i, Role: "user", Text: txt}
	}
	if err := s.ApplyRawImport(meta, recs, "", "", true); err != nil {
		t.Fatalf("ApplyRawImport(replace): %v", err)
	}
}

// TestCommitDoesNotAdvanceWatermarkWhenRawReplacedMidMine reproduces issue #49 C2:
// a worker mines a session, then a replace-import (OpenCode history rewrite) deletes
// and re-inserts that session's raw UNDER the mine, then the worker commits. The
// stale count must NOT be blind-written over the reset progress row — else the
// re-imported turns are silently marked distilled and never mined. The guard
// (MarkDistilledIfCurrent, keyed on the mined raw.id still existing) holds the
// watermark so the new generation stays pending. OpenCode-triggered path.
func TestCommitDoesNotAdvanceWatermarkWhenRawReplacedMidMine(t *testing.T) {
	s := newStore(t)
	m := &fakeMiner{}
	w := testWorker(s, m)
	ctx := context.Background()
	sess := "opencode:s"

	// Original generation: 2 turns.
	capture(t, s, sess, "user", "original one")
	capture(t, s, sess, "assistant", "original two")

	// MAP: mine the original generation (captures RawHighID at read time).
	mining, err := w.MineSession(ctx, sess)
	if err != nil {
		t.Fatal(err)
	}
	if mining.Total != 2 {
		t.Fatalf("mined Total = %d, want 2", mining.Total)
	}

	// RACE: a replace-import lands before the commit — 3 brand-new (edited) turns.
	replaceRaw(t, s, sess, []string{"edited one", "edited two", "edited three"})

	// REDUCE: commit the stale mining result.
	existing, _ := s.ReadObservations("")
	if err := w.CommitMining(mining, &existing); err != nil {
		t.Fatal(err)
	}

	// The watermark must NOT have been advanced to the stale count of 2 over the
	// replaced generation. Progress was reset by the replace-import (absent → 0), so
	// the guard leaves it at 0 and the session stays fully pending.
	if got := s.DistilledCount(sess, "default"); got != 0 {
		t.Fatalf("watermark advanced to %d over a replaced generation; want 0 (session must re-mine)", got)
	}
	pending, _ := s.PendingSessions(nil)
	found := false
	for _, p := range pending {
		if p == sess {
			found = true
		}
	}
	if !found {
		t.Fatal("session with a replaced raw generation must be pending for a re-mine")
	}

	// And a subsequent clean pass re-mines the NEW generation end-to-end.
	if err := w.Process(ctx, sess); err != nil {
		t.Fatal(err)
	}
	if got := s.DistilledCount(sess, "default"); got != 3 {
		t.Fatalf("after re-mine, watermark = %d, want 3 (the new generation)", got)
	}
}

// TestCommitAdvancesWatermarkOnAppendOnlyPath is the both-paths guarantee: on the
// normal append-only path (Claude Code capture never deletes raw), the CAS guard
// always passes and the watermark advances exactly as before — the fix is free
// insurance there, never a false block. Also covers an append landing mid-mine
// (a resume): the mined generation's raw.id still exists, so the watermark advances
// to what was mined and the appended turns remain pending, as intended.
func TestCommitAdvancesWatermarkOnAppendOnlyPath(t *testing.T) {
	s := newStore(t)
	m := &fakeMiner{}
	w := testWorker(s, m)
	ctx := context.Background()
	sess := "claude:s" // append-only runtime

	capture(t, s, sess, "user", "one")
	capture(t, s, sess, "assistant", "two")

	mining, err := w.MineSession(ctx, sess)
	if err != nil {
		t.Fatal(err)
	}

	// A resume APPENDS a turn mid-mine (does not delete). The mined high id still
	// exists, so the guard passes and the watermark advances to the mined count.
	capture(t, s, sess, "user", "three (arrived during mine)")

	existing, _ := s.ReadObservations("")
	if err := w.CommitMining(mining, &existing); err != nil {
		t.Fatal(err)
	}
	if got := s.DistilledCount(sess, "default"); got != 2 {
		t.Fatalf("append-only watermark = %d, want 2 (the mined count advances cleanly)", got)
	}
	// The appended 3rd turn is past the watermark → still pending, as designed.
	pending, _ := s.PendingSessions(nil)
	found := false
	for _, p := range pending {
		if p == sess {
			found = true
		}
	}
	if !found {
		t.Fatal("the turn appended during the mine should leave the session pending")
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
	if got := s.DistilledCount("s", "default"); got != 0 {
		t.Fatalf("watermark must NOT advance on failure, got %d", got)
	}
	if got := s.RetryCount("s", "default"); got != 1 {
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

	if got := s.DistilledCount("s", "default"); got != 0 {
		t.Fatalf("watermark must NEVER advance while mining fails, got %d", got)
	}
	if obs, _ := s.ReadObservations(""); len(obs) != 0 {
		t.Fatalf("nothing should be written on persistent failure, got %d obs", len(obs))
	}
	if got := s.RetryCount("s", "default"); got != 5 {
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
	p, _ := s.PendingSessions(nil)
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
	if got := s.DistilledCount("s", "default"); got != 2 {
		t.Fatalf("quiet session should advance watermark to 2, got %d", got)
	}
	if got := s.RetryCount("s", "default"); got != 0 {
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
	if got := s.DistilledCount("s", "default"); got != 2 {
		t.Fatalf("watermark should advance after success, got %d", got)
	}
	if got := s.RetryCount("s", "default"); got != 0 {
		t.Fatalf("retry count should reset after success, got %d", got)
	}
}

// lensRouter is a Run seam that dispatches by extract prompt: each lens's
// ln.Extract string selects behavior, so a test can make ONE lens fail while
// others succeed (per-lens failure isolation, issue #55).
type lensRouter struct {
	inputsByPrompt map[string][]string
	failPrompt     string // the ln.Extract of the lens that should error
}

func (r *lensRouter) run(_ context.Context, _ string, prompt string, input string) (string, error) {
	if r.inputsByPrompt == nil {
		r.inputsByPrompt = map[string][]string{}
	}
	r.inputsByPrompt[prompt] = append(r.inputsByPrompt[prompt], input)
	if prompt == r.failPrompt {
		return "", fmt.Errorf("simulated failure for lens prompt %q", prompt)
	}
	arr := []minedObs{{Dimension: "thinking", Observation: "obs(" + prompt + "):" + input, Evidence: "e", Poignancy: 3}}
	b, _ := json.Marshal(arr)
	return string(b), nil
}

func twoLensWorker(s *store.Store, r *lensRouter) *Worker {
	return &Worker{
		Store:    s,
		Embedder: fakeEmbedder{},
		Lenses: []*lens.Lens{
			{Name: "default", Global: true, Extract: "mine-default", Dimensions: []string{"thinking"}},
			{Name: "codereview", Extract: "mine-codereview", Dimensions: []string{"thinking"}},
		},
		Config: store.Config{},
		Run:    r.run,
	}
}

// #55: enabling a NEW lens over an already-default-distilled session must mine ONLY
// the new lens — default is at its watermark and must not be re-mined.
func TestNewLensMinesOnlyItselfOverDistilledSession(t *testing.T) {
	s := newStore(t)
	capture(t, s, "s", "user", "alpha")
	capture(t, s, "s", "assistant", "reply")

	// First pass: default only, catches up to 2.
	single := testWorker(s, &fakeMiner{})
	if err := single.Process(context.Background(), "s"); err != nil {
		t.Fatalf("default pass: %v", err)
	}
	if got := s.DistilledCount("s", "default"); got != 2 {
		t.Fatalf("default should be at 2, got %d", got)
	}

	// Second pass: default + codereview now active. default is caught up (mines
	// nothing); codereview is at 0 and mines the whole session.
	r := &lensRouter{}
	if err := twoLensWorker(s, r).Process(context.Background(), "s"); err != nil {
		t.Fatalf("two-lens pass: %v", err)
	}
	if len(r.inputsByPrompt["mine-default"]) != 0 {
		t.Fatalf("default is caught up; it must NOT be re-mined, got %d calls", len(r.inputsByPrompt["mine-default"]))
	}
	if len(r.inputsByPrompt["mine-codereview"]) != 1 {
		t.Fatalf("codereview must mine the session once, got %d calls", len(r.inputsByPrompt["mine-codereview"]))
	}
	if got := s.DistilledCount("s", "codereview"); got != 2 {
		t.Fatalf("codereview watermark should advance to 2, got %d", got)
	}
	if got := s.DistilledCount("s", "default"); got != 2 {
		t.Fatalf("default watermark must stay 2, got %d", got)
	}
	// codereview produced its observation; default's earlier one is still there.
	obs, _ := s.ReadObservations("codereview")
	if len(obs) != 1 {
		t.Fatalf("codereview should have 1 observation, got %d", len(obs))
	}
}

// #55: when one lens's mine fails but a sibling succeeds, the healthy lens must
// commit + advance while ONLY the failed lens backs off — the old all-or-nothing
// rule discarded the healthy lens too.
func TestPerLensFailureIsolation(t *testing.T) {
	s := newStore(t)
	capture(t, s, "s", "user", "alpha")
	capture(t, s, "s", "assistant", "reply")

	r := &lensRouter{failPrompt: "mine-codereview"} // codereview fails, default succeeds
	w := twoLensWorker(s, r)
	if err := w.Process(context.Background(), "s"); err != nil {
		t.Fatalf("Process: %v", err)
	}

	// default committed and advanced.
	if got := s.DistilledCount("s", "default"); got != 2 {
		t.Fatalf("healthy default lens must advance to 2, got %d", got)
	}
	if got := s.RetryCount("s", "default"); got != 0 {
		t.Fatalf("healthy lens must not accrue retries, got %d", got)
	}
	if obs, _ := s.ReadObservations("default"); len(obs) != 1 {
		t.Fatalf("healthy lens's observation must be written, got %d", len(obs))
	}
	// codereview failed: watermark held at 0, retry counted, backed off.
	if got := s.DistilledCount("s", "codereview"); got != 0 {
		t.Fatalf("failed lens watermark must hold at 0, got %d", got)
	}
	if got := s.RetryCount("s", "codereview"); got != 1 {
		t.Fatalf("failed lens must count a retry, got %d", got)
	}
	if obs, _ := s.ReadObservations("codereview"); len(obs) != 0 {
		t.Fatalf("failed lens must write nothing, got %d", len(obs))
	}

	// A recovery pass (codereview no longer fails) catches it up without touching
	// default (already at watermark).
	r2 := &lensRouter{}
	if err := twoLensWorker(s, r2).Process(context.Background(), "s"); err != nil {
		t.Fatalf("recovery Process: %v", err)
	}
	if len(r2.inputsByPrompt["mine-default"]) != 0 {
		t.Fatalf("default already caught up; must not re-mine on recovery, got %d", len(r2.inputsByPrompt["mine-default"]))
	}
	if got := s.DistilledCount("s", "codereview"); got != 2 {
		t.Fatalf("codereview must catch up on recovery, got %d", got)
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
	if got := s.DistilledCount("s", "default"); got != 4 {
		t.Errorf("watermark should be 4 after distilling all turns, got %d", got)
	}
}
