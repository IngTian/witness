package distill

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/IngTian/witness/internal/lens"
	"github.com/IngTian/witness/internal/store"
)

// Lens-parity suite (issue #55 follow-up / lens-parity audit PR 4).
//
// The governing invariant: a lens is just a prompt + a name, and the distillation
// engine must treat EVERY lens identically. "default" gets legitimate specialness
// only in IDENTITY (always-on, built-in prompt source, reserved name) — never in
// BEHAVIOR. These tests run the SAME input through the built-in "default" lens and
// a synthetic registered lens ("parity") and assert identical engine behavior at
// each stage (mine, watermark, dedup, review→facets, backoff). If a future change
// re-special-cases default behaviorally, one of these fails.
//
// The prompts are deliberately structural, not real — the fake miner routes on the
// extract-prompt string, so any observed difference between the two lenses is a
// CODE-PATH difference, not a prompt-content difference.

const parityLens = "parity"

// parityMiner is a MineFunc that behaves identically for every lens: it emits one
// observation per call whose text is derived ONLY from the input transcript (NOT
// the lens), so the two lenses produce the same observation TEXT for the same
// delta — letting us assert that only the lens TAG (and thus obsID) differs.
// failFor names a lens whose extract prompt should error (per-lens failure tests).
type parityMiner struct {
	callsByPrompt map[string]int
	failFor       string // the ln.Extract that should fail
}

func (m *parityMiner) run(_ context.Context, _ string, prompt, input string) (string, error) {
	if m.callsByPrompt == nil {
		m.callsByPrompt = map[string]int{}
	}
	m.callsByPrompt[prompt]++
	if prompt == m.failFor {
		return "", fmt.Errorf("simulated failure for prompt %q", prompt)
	}
	// Observation text depends only on the transcript — identical across lenses.
	arr := []minedObs{{Dimension: "thinking", Observation: "obs:" + input, Evidence: "e", Poignancy: 4}}
	b, _ := json.Marshal(arr)
	return string(b), nil
}

// parityWorker wires default + a registered "parity" lens with structurally
// identical prompts (distinct extract strings only so the miner can route/fail per
// lens). Both are ordinary entries in w.Lenses — default is NOT flagged/handled
// specially by the engine.
func parityWorker(s *store.Store, m *parityMiner) *Worker {
	return &Worker{
		Store:    s,
		Embedder: fakeEmbedder{},
		Lenses: []*lens.Lens{
			{Name: store.LensDefault, BuiltIn: true, Extract: "extract-default", Review: "review-default", Dimensions: []string{"thinking"}},
			{Name: parityLens, BuiltIn: false, Extract: "extract-parity", Review: "review-parity", Dimensions: []string{"thinking"}},
		},
		Config: store.Config{},
		Run:    m.run,
	}
}

// MINE + WATERMARK parity: over one session, both lenses mine the same delta, the
// same number of observations, and end at the same watermark. The obs TEXT is
// identical; only the lens tag (and thus obsID) differs — proving the mine loop is
// lens-agnostic.
func TestParityMineAndWatermark(t *testing.T) {
	s := newStore(t)
	capture(t, s, "s", "user", "alpha")
	capture(t, s, "s", "assistant", "reply")

	m := &parityMiner{}
	if err := parityWorker(s, m).Process(context.Background(), "s"); err != nil {
		t.Fatalf("Process: %v", err)
	}

	// Each lens's extract prompt was invoked the same number of times (same delta).
	if m.callsByPrompt["extract-default"] != m.callsByPrompt["extract-parity"] {
		t.Fatalf("lenses mined a different number of times: default=%d parity=%d",
			m.callsByPrompt["extract-default"], m.callsByPrompt["extract-parity"])
	}
	// Both watermarks advanced identically.
	if d, p := s.DistilledCount("s", store.LensDefault), s.DistilledCount("s", parityLens); d != p || d != 2 {
		t.Fatalf("watermarks must be equal and =2: default=%d parity=%d", d, p)
	}
	// Both produced the same observation TEXT, differing only by lens tag.
	def, _ := s.ReadObservations(store.LensDefault)
	par, _ := s.ReadObservations(parityLens)
	if len(def) != 1 || len(par) != 1 {
		t.Fatalf("each lens should have 1 observation: default=%d parity=%d", len(def), len(par))
	}
	if def[0].Observation != par[0].Observation {
		t.Fatalf("same delta must yield same obs text: default=%q parity=%q", def[0].Observation, par[0].Observation)
	}
	if def[0].ID == par[0].ID {
		t.Fatalf("obsID must be lens-namespaced (differ), got identical: %q", def[0].ID)
	}
	if def[0].Lens != store.LensDefault || par[0].Lens != parityLens {
		t.Fatalf("lens tags wrong: default=%q parity=%q", def[0].Lens, par[0].Lens)
	}
}

// WATERMARK independence: a lens enabled AFTER default is caught up backfills the
// whole session for itself without disturbing default (#55) — and this holds for a
// registered lens exactly as it would if the roles were reversed.
func TestParityWatermarkIndependence(t *testing.T) {
	s := newStore(t)
	capture(t, s, "s", "user", "alpha")
	capture(t, s, "s", "assistant", "reply")

	// Phase 1: only default active (parity not yet enabled) — default catches up.
	defOnly := parityWorker(s, &parityMiner{})
	defOnly.Lenses = defOnly.Lenses[:1] // just default
	if err := defOnly.Process(context.Background(), "s"); err != nil {
		t.Fatalf("default-only pass: %v", err)
	}
	if s.DistilledCount("s", store.LensDefault) != 2 {
		t.Fatalf("default should be caught up at 2")
	}

	// The session is not pending when only default is active...
	if p, _ := s.PendingSessions([]string{store.LensDefault}); contains(p, "s") {
		t.Fatalf("caught-up default should not be pending")
	}
	// ...but IS pending once parity is active (parity behind at 0) — without a full reset.
	if p, _ := s.PendingSessions([]string{store.LensDefault, parityLens}); !contains(p, "s") {
		t.Fatalf("a newly-active lens must make the session pending again")
	}

	// Phase 2: both active — parity mines the whole session, default is untouched.
	m := &parityMiner{}
	if err := parityWorker(s, m).Process(context.Background(), "s"); err != nil {
		t.Fatalf("both-lens pass: %v", err)
	}
	if m.callsByPrompt["extract-default"] != 0 {
		t.Fatalf("caught-up default must NOT be re-mined, got %d calls", m.callsByPrompt["extract-default"])
	}
	if m.callsByPrompt["extract-parity"] != 1 {
		t.Fatalf("parity must mine the session once, got %d", m.callsByPrompt["extract-parity"])
	}
	if d, p := s.DistilledCount("s", store.LensDefault), s.DistilledCount("s", parityLens); d != 2 || p != 2 {
		t.Fatalf("both watermarks should end at 2: default=%d parity=%d", d, p)
	}
}

// DEDUP self-scoping: dedup is per-lens, so a default observation never suppresses a
// parity observation with identical text and vice-versa. Both lenses keep their obs.
func TestParityDedupIsSelfScoped(t *testing.T) {
	s := newStore(t)
	capture(t, s, "s", "user", "alpha")
	capture(t, s, "s", "assistant", "reply")

	// Both lenses produce "obs:<same transcript>" — identical text, different lens.
	m := &parityMiner{}
	if err := parityWorker(s, m).Process(context.Background(), "s"); err != nil {
		t.Fatalf("Process: %v", err)
	}
	// If dedup were lens-blind, one lens's obs would suppress the other's. Both survive.
	if d, _ := s.ReadObservations(store.LensDefault); len(d) != 1 {
		t.Fatalf("default obs suppressed by parity (dedup not self-scoped): got %d", len(d))
	}
	if p, _ := s.ReadObservations(parityLens); len(p) != 1 {
		t.Fatalf("parity obs suppressed by default (dedup not self-scoped): got %d", len(p))
	}
}

// REVIEW→FACETS parity: the reviewer processes both lenses uniformly; facets are
// namespaced per lens with identical structure. A per-lens review FAILURE blocks the
// global stamp regardless of WHICH lens failed (default or parity behave the same).
func TestParityReviewAndFacets(t *testing.T) {
	// 4a: both lenses reviewed → facets for each, namespaced by lens.
	s := newStore(t)
	for _, ln := range []string{store.LensDefault, parityLens} {
		if err := s.AppendObservations([]store.Observation{{
			ID: obsID("sess", ln, "o-"+ln), Session: "sess", Lens: ln,
			Dimension: "thinking", Observation: "did a thing", Poignancy: 5,
		}}); err != nil {
			t.Fatalf("seed obs %s: %v", ln, err)
		}
	}
	facetReply := func(_ context.Context, _, _, _ string) (string, error) {
		return `[{"dimension":"thinking","key":"clarity","value":"improving","confidence":0.9,"because_of":["x"],"contradicts_prior":false}]`, nil
	}
	r := &Reviewer{Store: s, Config: store.Config{}, Runner: facetReply, Lenses: []*lens.Lens{
		{Name: store.LensDefault, Review: "review-default"},
		{Name: parityLens, Review: "review-parity"},
	}}
	if err := r.Run(context.Background(), time.Now()); err != nil {
		t.Fatalf("review: %v", err)
	}
	facets, _ := s.ReadFacets()
	byLens := map[string]int{}
	for _, f := range facets {
		byLens[f.Lens]++
	}
	if byLens[store.LensDefault] != 1 || byLens[parityLens] != 1 {
		t.Fatalf("each lens should get one facet: %v", byLens)
	}

	// 4b: a parity review FAILURE blocks the stamp exactly as a default failure would.
	assertFailingLensBlocksStamp(t, parityLens)
	assertFailingLensBlocksStamp(t, store.LensDefault)
}

// assertFailingLensBlocksStamp: when the named lens's review call errors, Run must
// return an error and NOT stamp the review — identical behavior whichever lens fails.
func assertFailingLensBlocksStamp(t *testing.T, failing string) {
	t.Helper()
	s := newStore(t)
	for _, ln := range []string{store.LensDefault, parityLens} {
		_ = s.AppendObservations([]store.Observation{{
			ID: obsID("sess", ln, "o-"+ln), Session: "sess", Lens: ln,
			Dimension: "thinking", Observation: "x", Poignancy: 3,
		}})
	}
	runner := func(_ context.Context, _, prompt, _ string) (string, error) {
		if prompt == "review-"+failing {
			return "", fmt.Errorf("simulated review failure")
		}
		return `[{"dimension":"thinking","key":"k","value":"v","confidence":0.9,"because_of":["x"],"contradicts_prior":false}]`, nil
	}
	r := &Reviewer{Store: s, Config: store.Config{}, Runner: runner, Lenses: []*lens.Lens{
		{Name: store.LensDefault, Review: "review-" + store.LensDefault},
		{Name: parityLens, Review: "review-" + parityLens},
	}}
	if err := r.Run(context.Background(), time.Now()); err == nil {
		t.Fatalf("a failing %q review must return an error", failing)
	}
	if s.MetaString("review_ts") != "" {
		t.Fatalf("a failing %q review must NOT stamp the review", failing)
	}
}

// BACKOFF parity: a transport failure produces a per-(session,lens) backoff row, and
// the sequence is structurally identical whichever lens fails — the failed lens is
// parked while the healthy sibling commits and advances.
func TestParityBackoffSymmetry(t *testing.T) {
	for _, failing := range []string{parityLens, store.LensDefault} {
		t.Run(failing, func(t *testing.T) {
			s := newStore(t)
			capture(t, s, "s", "user", "alpha")
			capture(t, s, "s", "assistant", "reply")

			failPrompt := "extract-" + failing
			healthy := store.LensDefault
			if failing == store.LensDefault {
				healthy = parityLens
			}
			m := &parityMiner{failFor: failPrompt}
			if err := parityWorker(s, m).Process(context.Background(), "s"); err != nil {
				t.Fatalf("Process: %v", err)
			}
			// Failed lens: watermark held at 0, retry counted, backed off.
			if got := s.DistilledCount("s", failing); got != 0 {
				t.Fatalf("failed lens %q watermark must hold at 0, got %d", failing, got)
			}
			if got := s.RetryCount("s", failing); got != 1 {
				t.Fatalf("failed lens %q must count a retry, got %d", failing, got)
			}
			// Healthy sibling: committed + advanced, no retry.
			if got := s.DistilledCount("s", healthy); got != 2 {
				t.Fatalf("healthy lens %q must advance to 2, got %d", healthy, got)
			}
			if got := s.RetryCount("s", healthy); got != 0 {
				t.Fatalf("healthy lens %q must not accrue retries, got %d", healthy, got)
			}
			// The failed lens is parked; the healthy one keeps the session pending.
			if p, _ := s.PendingSessions([]string{failing}); contains(p, "s") {
				t.Fatalf("failed lens %q alone should be backed off / not pending", failing)
			}
			if p, _ := s.PendingSessions([]string{healthy}); contains(p, "s") {
				t.Fatalf("healthy lens %q is caught up → not pending", healthy)
			}
		})
	}
}

func contains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}
