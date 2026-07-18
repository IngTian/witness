package store

import (
	"sync"
	"testing"
)

// replaceGen simulates a replace-import (ApplyRawImport replace=true): DELETE the
// session's raw + progress and re-INSERT a fresh generation of n turns. raw.id is
// AUTOINCREMENT, so the new rows always get strictly higher ids than any prior
// generation. Returns the error (no *testing.T) so it is safe to call from a
// spawned goroutine, where t.Fatalf must not be used.
func replaceGen(s *Store, session string, n int) error {
	recs := make([]RawRecord, n)
	for i := range recs {
		recs[i] = RawRecord{Session: session, Seq: i, Role: "user", Text: "x"}
	}
	return s.ApplyRawImport(SessionMeta{Session: session}, recs, "", "", true)
}

// TestReadRawSnapshotContract locks the atomic-snapshot contract issue #67-1 relies
// on: the records and rawHighID come from ONE read, so rawHighID is the max id of
// exactly those records — and, on a stable database, it agrees with the standalone
// MaxRawID/ReadRaw primitives (which the method must never silently diverge from).
func TestReadRawSnapshotContract(t *testing.T) {
	s := tempStore(t)

	// Empty session: no rows → (nil, 0, nil), matching MaxRawID's COALESCE(MAX(id),0).
	recs, high, err := s.ReadRawSnapshot("sess")
	if err != nil {
		t.Fatalf("ReadRawSnapshot(empty): %v", err)
	}
	if len(recs) != 0 || high != 0 {
		t.Fatalf("empty session: want (0 recs, high 0), got (%d recs, high %d)", len(recs), high)
	}

	// After appends: count + high agree with the standalone primitives, and the
	// records match ReadRaw field-for-field (same ORDER BY id, so same order).
	appendN(t, s, "sess", 5)
	recs, high, err = s.ReadRawSnapshot("sess")
	if err != nil {
		t.Fatalf("ReadRawSnapshot: %v", err)
	}
	if len(recs) != 5 {
		t.Fatalf("want 5 records, got %d", len(recs))
	}
	if want := s.MaxRawID("sess"); high != want {
		t.Fatalf("snapshot high %d != MaxRawID %d", high, want)
	}
	plain, err := s.ReadRaw("sess")
	if err != nil {
		t.Fatalf("ReadRaw: %v", err)
	}
	if len(plain) != len(recs) {
		t.Fatalf("ReadRawSnapshot returned %d recs, ReadRaw %d", len(recs), len(plain))
	}
	for i := range plain {
		if plain[i] != recs[i] { // RawRecord is all comparable fields
			t.Fatalf("record %d mismatch: snapshot %+v vs ReadRaw %+v", i, recs[i], plain[i])
		}
	}
}

// TestSplitReadTornPairWouldBlindAdvance documents issue #67-1 concretely and proves
// the fix closes it. The OLD mine read count/content (ReadRaw) and the generation id
// (MaxRawID) as TWO separate statements. MaxOpenConns(1) serializes each but releases
// the connection between them, so a replace-import committing in that gap yields a
// TORN pair — an old-generation count with the new generation's high id. The CAS
// (MarkDistilledIfCurrent) then finds that new id live, PASSES its guard, and blind-
// advances the watermark over never-mined turns. ReadRawSnapshot reads both in one
// query, so the pair can never straddle a generation boundary.
func TestSplitReadTornPairWouldBlindAdvance(t *testing.T) {
	s := tempStore(t)
	const session = "sess"

	// Generation 1: 5 turns.
	appendN(t, s, session, 5)

	// Reproduce the OLD split read with a replace-import landing in the gap:
	//   1. read count from generation 1 ...
	oldRecs, err := s.ReadRaw(session)
	if err != nil {
		t.Fatalf("ReadRaw: %v", err)
	}
	oldCount := len(oldRecs) // 5, from generation 1
	//   2. ... a replace-import commits (generation 2: 2 turns, strictly higher ids) ...
	if err := replaceGen(s, session, 2); err != nil {
		t.Fatalf("replaceGen: %v", err)
	}
	//   3. ... then read the high id — now from generation 2.
	tornHigh := s.MaxRawID(session)

	// The torn pair (gen-1 count, gen-2 high) is exactly the bug: the CAS guard asks
	// only "does a raw row with tornHigh still exist?" — it does (gen 2 is live) — so
	// the watermark blind-advances to 5 over a 2-turn generation: 3 turns are marked
	// distilled that were never mined. This assertion demonstrates the hazard the
	// split read exposes (it is NOT the fixed path).
	ok, err := s.MarkDistilledIfCurrent(session, LensDefault, oldCount, tornHigh)
	if err != nil {
		t.Fatalf("MarkDistilledIfCurrent(torn): %v", err)
	}
	if !ok || s.DistilledCount(session, LensDefault) != 5 {
		t.Fatalf("the torn split-read pair should (wrongly) advance to 5; ok=%v count=%d",
			ok, s.DistilledCount(session, LensDefault))
	}

	// ReadRawSnapshot can NEVER hand back that torn pair: its count and high always
	// describe ONE generation. After the same replace, one atomic read pairs the gen-2
	// count (2) with the gen-2 high — the truth the CAS needs.
	recs, high, err := s.ReadRawSnapshot(session)
	if err != nil {
		t.Fatalf("ReadRawSnapshot: %v", err)
	}
	if len(recs) != 2 || high != tornHigh {
		t.Fatalf("atomic snapshot must describe one generation: got %d recs, high %d (want 2 recs, high %d)",
			len(recs), high, tornHigh)
	}
}

// TestReadRawSnapshotAtomicUnderConcurrentReplace is the concurrency guard for issue
// #67-1: with a writer driving replace-imports and several readers hammering
// ReadRawSnapshot, EVERY returned pair must be self-consistent. raw.id is
// AUTOINCREMENT (never reused), so a high id uniquely identifies a committed
// generation; the writer records each committed high id's true record count, so any
// cross-generation (torn) pair — an old count with a new high — is caught the instant
// a reader observes it. The atomic single-query read never trips this; a split
// two-statement read (the old bug) could, since a replace can commit between the
// statements. MaxOpenConns(1) means reads and the replace serialize at the DB — the
// exact production concurrency model.
func TestReadRawSnapshotAtomicUnderConcurrentReplace(t *testing.T) {
	s := tempStore(t)
	const session = "sess"

	// Seed a first generation so readers have data before the writer starts.
	if err := replaceGen(s, session, 3); err != nil {
		t.Fatalf("seed replaceGen: %v", err)
	}
	var wantCount sync.Map // rawHighID (int64) -> true record count committed with it
	wantCount.Store(s.MaxRawID(session), 3)

	const rounds = 200
	stop := make(chan struct{})
	var wg sync.WaitGroup

	// Writer: alternate 7- and 3-turn generations via replace-import, recording each
	// committed high id's true count BEFORE moving on. A reader that observes a high id
	// not yet recorded simply skips it (no false failure); once recorded it is
	// authoritative, since the id is never reused.
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer close(stop)
		for i := 0; i < rounds; i++ {
			n := 3
			if i%2 == 0 {
				n = 7
			}
			if err := replaceGen(s, session, n); err != nil {
				t.Errorf("replaceGen round %d: %v", i, err) // Errorf is goroutine-safe; Fatalf is not
				return
			}
			wantCount.Store(s.MaxRawID(session), n)
		}
	}()

	// Readers: snapshot in a tight loop; assert every pair is one coherent generation.
	for r := 0; r < 4; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				recs, high, err := s.ReadRawSnapshot(session)
				if err != nil {
					t.Errorf("ReadRawSnapshot: %v", err)
					return
				}
				// count == 0 must coincide with high == 0 (never a half-empty read).
				if (len(recs) == 0) != (high == 0) {
					t.Errorf("torn empty state: %d recs but high %d", len(recs), high)
					return
				}
				// The count paired with this high must be the one committed for that
				// generation. A cross-generation (torn) pairing fails here.
				if v, ok := wantCount.Load(high); ok {
					if want := v.(int); len(recs) != want {
						t.Errorf("torn snapshot: high %d was committed with %d records, snapshot saw %d",
							high, want, len(recs))
						return
					}
				}
			}
		}()
	}
	wg.Wait()
}
