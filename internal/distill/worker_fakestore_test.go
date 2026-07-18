package distill

import (
	"context"
	"testing"
	"time"

	"github.com/IngTian/witness/internal/lens"
	"github.com/IngTian/witness/internal/store"
)

// fakeQueue is a hand-written store.Queue backed by plain in-memory maps — NO real
// *sql.DB. It is the concrete proof of issue #73-C1: the distillation Worker drives its
// entire mine → commit → watermark cycle through the narrow store.Queue interface, so it
// can run with no database at all. Only the methods the Worker/drain actually call have
// real behavior; the rest satisfy the interface as minimal stubs.
type fakeQueue struct {
	raw       map[string][]store.RawRecord // session -> L0 (in capture order)
	rawHigh   map[string]int64             // session -> highest raw "generation" id
	staged    map[string][]store.Observation
	obs       []store.Observation       // appended L1
	distilled map[string]map[string]int // session -> lens -> watermark
	platform  map[string]string         // session -> owning platform ("" = unknown)

	appendCalls int
	drifts      []string
}

func newFakeQueue() *fakeQueue {
	return &fakeQueue{
		raw:       map[string][]store.RawRecord{},
		rawHigh:   map[string]int64{},
		staged:    map[string][]store.Observation{},
		distilled: map[string]map[string]int{},
		platform:  map[string]string{},
	}
}

// --- inputs ---
func (q *fakeQueue) ReadRawSnapshot(session string) ([]store.RawRecord, int64, error) {
	return q.raw[session], q.rawHigh[session], nil
}
func (q *fakeQueue) RawCount(session string) int { return len(q.raw[session]) }
func (q *fakeQueue) ReadObservations(lens string) ([]store.Observation, error) {
	if lens == "" {
		return q.obs, nil
	}
	var out []store.Observation
	for _, o := range q.obs {
		if o.Lens == lens {
			out = append(out, o)
		}
	}
	return out, nil
}
func (q *fakeQueue) DrainStaged(session string) ([]store.Observation, int64, error) {
	s := q.staged[session]
	return s, int64(len(s)), nil
}
func (q *fakeQueue) SessionPlatform(session string) string { return q.platform[session] }

// --- watermark / readiness ---
func (q *fakeQueue) DistilledCount(session, lens string) int {
	if m := q.distilled[session]; m != nil {
		return m[lens]
	}
	return 0
}
func (q *fakeQueue) LensBackedOff(session, lens string, now time.Time) bool { return false }
func (q *fakeQueue) PendingInputChars(session string, lenses []string) int {
	var n int
	for _, r := range q.raw[session] {
		n += len(r.Text)
	}
	return n
}

// --- commit + advance ---
func (q *fakeQueue) AppendObservations(obs []store.Observation) error {
	q.appendCalls++
	q.obs = append(q.obs, obs...)
	return nil
}
func (q *fakeQueue) MarkDistilledIfCurrent(session, lens string, count int, rawHighID int64) (bool, error) {
	// Mimic the real CAS: advance only if the mined generation is still the current
	// high id (it always is in this single-threaded test).
	if rawHighID != q.rawHigh[session] {
		return false, nil
	}
	if q.distilled[session] == nil {
		q.distilled[session] = map[string]int{}
	}
	q.distilled[session][lens] = count
	return true, nil
}
func (q *fakeQueue) ClearStagedThrough(session string, throughID int64) {
	if throughID <= 0 {
		return
	}
	q.staged[session] = nil
}

// --- retry / backoff / drift (unused by the happy path, minimal stubs) ---
func (q *fakeQueue) IncRetry(session, lens string) int              { return 1 }
func (q *fakeQueue) ResetRetry(session, lens string)                {}
func (q *fakeQueue) SetNextAttempt(s, l string, at time.Time) error { return nil }
func (q *fakeQueue) SetDrift(session, lens string) error            { return nil }
func (q *fakeQueue) RecordDrift(n int, session, lens string) error {
	q.drifts = append(q.drifts, lens)
	return nil
}

var _ store.Queue = (*fakeQueue)(nil)

// TestWorkerMinesAgainstFakeQueue drives a full MineSession → CommitMining cycle with a
// fakeQueue and NO *sql.DB, proving the Worker depends only on store.Queue (issue
// #73-C1). It seeds two raw turns, mines with the package's fakeMiner/fakeEmbedder, and
// asserts an L1 observation was appended and the default lens's watermark advanced to the
// full raw count.
func TestWorkerMinesAgainstFakeQueue(t *testing.T) {
	q := newFakeQueue()
	const sess = "s-fake"
	q.raw[sess] = []store.RawRecord{
		{Session: sess, Seq: 0, Role: "user", Text: "how do I center a div?", TS: "2026-07-18T00:00:00Z"},
		{Session: sess, Seq: 1, Role: "assistant", Text: "use flexbox", TS: "2026-07-18T00:00:01Z"},
	}
	q.rawHigh[sess] = 2

	miner := &fakeMiner{}
	w := &Worker{
		Store:    q,
		Embedder: fakeEmbedder{},
		Lenses:   []*lens.Lens{{Name: "default", BuiltIn: true, Extract: "mine", Dimensions: []string{"thinking"}}},
		Config:   store.Config{},
		Run:      miner.run,
	}

	ctx := context.Background()
	mining, err := w.MineSession(ctx, sess)
	if err != nil {
		t.Fatalf("MineSession against fake queue: %v", err)
	}
	if mining.NothingToDo {
		t.Fatal("expected a delta to mine, got NothingToDo")
	}
	var existing []store.Observation
	if err := w.CommitMining(mining, &existing); err != nil {
		t.Fatalf("CommitMining against fake queue: %v", err)
	}

	if q.appendCalls == 0 || len(q.obs) == 0 {
		t.Fatalf("expected L1 observations appended, got %d obs in %d calls", len(q.obs), q.appendCalls)
	}
	if got := q.DistilledCount(sess, "default"); got != 2 {
		t.Fatalf("watermark for default lens = %d, want 2 (full raw count)", got)
	}
	// The miner saw the whole session as one transcript (default chunk policy = whole).
	if len(miner.inputs) != 1 {
		t.Fatalf("miner inputs = %d, want 1 (whole-session default policy)", len(miner.inputs))
	}
}
