package commands

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/IngTian/witness/internal/store"
)

// openSeedTestStore opens a store in the given WITNESS_HOME with the bundled prompts
// reachable (so lens.DefaultSeedDir resolves during the seed hook). Since #102 the
// first-open hook AUTO-SEEDS the default lens on a fresh archive, so opening a brand-new
// home leaves default registered+enabled. Returns the store; the home is the caller's so
// it can re-open the SAME archive to exercise the one-shot.
func openSeedTestStore(t *testing.T, home string) *store.Store {
	t.Helper()
	t.Setenv("WITNESS_HOME", home)
	t.Setenv("WITNESS_PROMPTS", filepath.Join("..", "..", "prompts"))
	t.Setenv(noDefaultLensEnv, "") // ensure a leftover opt-out from an earlier open doesn't leak
	s, err := store.Open()
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// openOptedOut opens a FRESH archive with WITNESS_NO_DEFAULT_LENS set, so the first-open
// hook records the opt-out and seeds nothing — the clean slate a library/service install
// starts from, and the way these tests build a "no default yet" state to migrate from.
// The env stays set until the caller clears it with t.Setenv(noDefaultLensEnv, "").
func openOptedOut(t *testing.T, home string) *store.Store {
	t.Helper()
	t.Setenv("WITNESS_HOME", home)
	t.Setenv("WITNESS_PROMPTS", filepath.Join("..", "..", "prompts"))
	t.Setenv(noDefaultLensEnv, "1")
	s, err := store.Open()
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	return s
}

// TestFreshArchiveAutoSeedsDefault is the #102 first-open contract: opening a brand-new
// archive (no raw, no watermarks, empty registry — the state install/import/doctor find
// when they open the store before any capture) auto-seeds the built-in default lens,
// registered + enabled, so a new personal user gets a working setup with no explicit
// step. The one-shot is then recorded, so a second Open changes nothing.
func TestFreshArchiveAutoSeedsDefault(t *testing.T) {
	home := filepath.Join(t.TempDir(), "fresh")
	s := openSeedTestStore(t, home)
	if !slices.Contains(s.RegisteredLenses(), store.LensDefault) {
		t.Fatal("a fresh archive must auto-seed default into the registry on first Open")
	}
	if !slices.Contains(s.LoadConfig().EnabledLenses, store.LensDefault) {
		t.Fatal("auto-seed must ENABLE default so it distills")
	}
	// The seeded lens carries the person dimensions from bundled prompts/default/lens.json.
	act, err := activeLenses(s)
	if err != nil || len(act) != 1 || act[0].Name != store.LensDefault {
		t.Fatalf("activeLenses after auto-seed = %v (err %v), want just default", act, err)
	}
	if len(act[0].Dimensions) == 0 {
		t.Fatal("auto-seeded default must carry its dimensions from the seeded lens.json")
	}
	// Idempotent: re-open the SAME archive → default registered exactly once, still enabled.
	_ = s.Close()
	s2 := openSeedTestStore(t, home)
	count := 0
	for _, n := range s2.RegisteredLenses() {
		if n == store.LensDefault {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("default must be registered exactly once after re-open; got %d", count)
	}
}

// TestLibraryOptOutSkipsAndSticks is the #102 opt-out contract: an archive opened with
// WITNESS_NO_DEFAULT_LENS set (a library/service install, or a user who declines the
// person-growth starter) is NOT seeded, and the opt-out STICKS — a later Open with the
// env UNSET must not silently seed default, because the one-shot decision is durable.
// `witness lens load-default` remains the escape hatch (covered elsewhere).
func TestLibraryOptOutSkipsAndSticks(t *testing.T) {
	home := filepath.Join(t.TempDir(), "lib")
	s := openOptedOut(t, home)
	if slices.Contains(s.RegisteredLenses(), store.LensDefault) {
		t.Fatal("an opted-out archive must NOT auto-seed default")
	}
	if got := s.MetaString(defaultMigratedKey); got != "opted-out" {
		t.Fatalf("opt-out must record the one-shot as decided; sentinel = %q", got)
	}
	_ = s.Close()

	// Re-open WITHOUT the env → the durable one-shot must keep default absent.
	s2 := openSeedTestStore(t, home) // clears the env before opening
	if slices.Contains(s2.RegisteredLenses(), store.LensDefault) {
		t.Fatal("opt-out must STICK: default reappeared after re-opening without the env")
	}
}

// TestDefaultSeedMigratesPre1aArchive is the #44 slice-1a migration guard: an archive
// that distilled under the OLD always-on default lens (a progress row for lens
// "default", but no lenses/default/ registry dir) must, on the next Open, have default
// retro-registered + enabled — so an existing install keeps working after default loses
// its built-in status. Then it must be idempotent (a second Open does nothing new).
func TestDefaultSeedMigratesPre1aArchive(t *testing.T) {
	home := filepath.Join(t.TempDir(), "witness")
	// Build a clean "no default seeded yet" slate by opting the first Open out; then
	// simulate a genuine PRE-1a archive: real L0 + a distillation watermark under
	// "default" (what HasLegacyDefaultData keys on), and CLEAR the one-shot flag so the
	// re-open sees the true pre-1a state (legacy data present, never decided).
	s := openOptedOut(t, home)
	if err := s.AppendRaw(store.RawRecord{Session: "s", Seq: 0, Role: "user", Text: "hi"}); err != nil {
		t.Fatalf("AppendRaw: %v", err)
	}
	if err := s.MarkDistilled("s", store.LensDefault, 1); err != nil {
		t.Fatalf("MarkDistilled: %v", err)
	}
	if err := s.SetMetaString(defaultMigratedKey, ""); err != nil {
		t.Fatalf("clear migration flag: %v", err)
	}
	if slices.Contains(s.RegisteredLenses(), store.LensDefault) {
		t.Fatal("precondition: default should NOT be registered before migration")
	}
	_ = s.Close()

	// Re-open the SAME archive WITHOUT the opt-out env → the seed hook sees legacy default
	// data + no registry entry → migrates.
	s2 := openSeedTestStore(t, home)
	if !slices.Contains(s2.RegisteredLenses(), store.LensDefault) {
		t.Fatal("migration must retro-register the default lens into the registry")
	}
	if !slices.Contains(s2.LoadConfig().EnabledLenses, store.LensDefault) {
		t.Fatal("migration must ENABLE the default lens so the install keeps distilling it")
	}
	act, err := activeLenses(s2)
	if err != nil {
		t.Fatalf("activeLenses: %v", err)
	}
	var names []string
	for _, l := range act {
		names = append(names, l.Name)
	}
	if !slices.Contains(names, store.LensDefault) {
		t.Fatalf("activeLenses must include the migrated default; got %v", names)
	}

	// Idempotent: a further Open does not error and leaves default registered exactly once.
	_ = s2.Close()
	s3 := openSeedTestStore(t, home)
	count := 0
	for _, n := range s3.RegisteredLenses() {
		if n == store.LensDefault {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("default must be registered exactly once after re-open; got %d", count)
	}
}

// TestDeregisteredDefaultStaysGone is the regression guard for the adversarial finding
// that the seed RESURRECTED a deregistered default forever (the two gates disagreed:
// trigger keyed on ever-present progress rows, idempotency on registry presence). The
// one-shot meta flag makes it truly one-shot: after it runs once, deregistering +
// disabling default must STICK across re-opens — the "default is deletable" promise
// (#102). The default-progress rows persist (deregister doesn't clear them), so this
// specifically proves the gate is the FLAG, not those rows — and now also that a
// brand-new archive's auto-seed does not re-fire either.
func TestDeregisteredDefaultStaysGone(t *testing.T) {
	home := filepath.Join(t.TempDir(), "witness")
	// A fresh archive auto-seeds default and records the one-shot as done.
	s := openSeedTestStore(t, home)
	if err := s.AppendRaw(store.RawRecord{Session: "s", Seq: 0, Role: "user", Text: "hi"}); err != nil {
		t.Fatalf("AppendRaw: %v", err)
	}
	if err := s.MarkDistilled("s", store.LensDefault, 1); err != nil {
		t.Fatalf("MarkDistilled: %v", err)
	}
	if !slices.Contains(s.RegisteredLenses(), store.LensDefault) {
		t.Fatal("precondition: fresh open should have auto-seeded default")
	}

	// The user removes default entirely — the exact "I don't want this lens" action.
	if err := s.DisableLens(store.LensDefault); err != nil {
		t.Fatalf("DisableLens: %v", err)
	}
	if err := s.DeregisterLens(store.LensDefault); err != nil {
		t.Fatalf("DeregisterLens: %v", err)
	}
	// The legacy signal that used to (wrongly) trigger re-seeding still exists...
	if !s.HasLegacyDefaultData() {
		t.Fatal("precondition: legacy default progress rows should persist (deregister doesn't clear them)")
	}
	_ = s.Close()

	// ...but across TWO more Opens, default must STAY gone (flag gates the one-shot).
	s2 := openSeedTestStore(t, home)
	if slices.Contains(s2.RegisteredLenses(), store.LensDefault) {
		t.Fatal("BUG: deregistered default was resurrected in the registry on re-open")
	}
	if slices.Contains(s2.LoadConfig().EnabledLenses, store.LensDefault) {
		t.Fatal("BUG: deregistered default was re-enabled on re-open")
	}
	_ = s2.Close()
	s3 := openSeedTestStore(t, home)
	if slices.Contains(s3.RegisteredLenses(), store.LensDefault) {
		t.Fatal("BUG: deregistered default resurrected on the second re-open")
	}
}

// TestBackfillFreshDisabledDefaultFailsFast is the regression guard for the adversarial
// data-loss finding: `lens backfill default --fresh` on a DISABLED default used to bypass
// the registered+enabled precondition (an `if name != LensDefault` special-case), so it
// DeleteLensData'd default's observations+facets and then no-op'd the re-mine (the drain
// excludes inactive lenses) — irreversible loss. The special-case is gone; default is now
// subject to the same guard as any lens, so backfill --fresh fails fast BEFORE dropping.
func TestBackfillFreshDisabledDefaultFailsFast(t *testing.T) {
	home := filepath.Join(t.TempDir(), "witness")
	s := openSeedTestStore(t, home) // fresh open auto-seeds + enables default
	if err := s.AppendObservations([]store.Observation{{ID: "o1", Lens: store.LensDefault, Observation: "x", Poignancy: 3}}); err != nil {
		t.Fatalf("AppendObservations: %v", err)
	}
	if err := s.MarkDistilled("s", store.LensDefault, 1); err != nil {
		t.Fatalf("MarkDistilled: %v", err)
	}
	// Disable default → it's registered but NOT enabled.
	if err := s.DisableLens(store.LensDefault); err != nil {
		t.Fatalf("DisableLens: %v", err)
	}
	obsBefore, _ := s.ReadObservations("")

	// backfill --fresh must FAIL FAST (not enabled) and drop NOTHING.
	// assumeYes=true so the test can't hang on the confirm prompt — the not-enabled guard
	// fires before the prompt anyway, which is the point.
	err := lensBackfill(s, store.LensDefault, true, true)
	if err == nil {
		t.Fatal("backfill --fresh of a DISABLED default must fail fast, not proceed to delete data")
	}
	if !strings.Contains(err.Error(), "not enabled") {
		t.Fatalf("expected an 'enable it first' error, got: %v", err)
	}
	obsAfter, _ := s.ReadObservations("")
	if len(obsAfter) != len(obsBefore) || len(obsAfter) == 0 {
		t.Fatalf("backfill --fresh must NOT have dropped observations: before=%d after=%d", len(obsBefore), len(obsAfter))
	}
}

// TestBackfillFreshWorkerActiveFailsFast guards the #102-fix ordering: `backfill --fresh`
// must FAIL FAST when a distillation worker is already running, BEFORE dropping any data —
// otherwise it would DeleteLensData and then find runWorker returns ran=false (lock held),
// leaving the lens gutted with no re-mine. We stand in for a live worker by holding the
// WorkerLock (the sole liveness authority), then assert --fresh errors and drops nothing.
func TestBackfillFreshWorkerActiveFailsFast(t *testing.T) {
	home := filepath.Join(t.TempDir(), "witness")
	s := openSeedTestStore(t, home) // fresh open auto-seeds + enables default
	if err := s.AppendObservations([]store.Observation{{ID: "o1", Lens: store.LensDefault, Observation: "x", Poignancy: 3}}); err != nil {
		t.Fatalf("AppendObservations: %v", err)
	}
	obsBefore, _ := s.ReadObservations("")

	// Stand in for a running worker: hold the WorkerLock so WorkerActive() reports true.
	unlock, ok := s.WorkerLock()
	if !ok {
		t.Fatal("precondition: should have acquired the worker lock")
	}
	defer unlock()

	// --fresh must refuse (worker running) and drop NOTHING — even with assumeYes, since the
	// worker-active guard is checked before the confirm and before DeleteLensData.
	err := lensBackfill(s, store.LensDefault, true, true)
	if err == nil {
		t.Fatal("backfill --fresh while a worker is running must fail fast, not drop data it can't re-mine")
	}
	if !strings.Contains(err.Error(), "worker is running") {
		t.Fatalf("expected a 'worker is running' error, got: %v", err)
	}
	obsAfter, _ := s.ReadObservations("")
	if len(obsAfter) != len(obsBefore) || len(obsAfter) == 0 {
		t.Fatalf("worker-active --fresh must NOT have dropped observations: before=%d after=%d", len(obsBefore), len(obsAfter))
	}
}

// TestSeedDefaultLensRestoresFromAnyState guards the `witness lens load-default` restore
// path: seedDefaultLens must leave default registered AND enabled from ALL three states a
// user can reach — including the DISABLED case, which an old skip-if-registered guard
// silently no-op'd. This is the escape hatch that makes "default is deletable" safe: a
// deliberate delete sticks (TestDeregisteredDefaultStaysGone), and this is how you undo it.
func TestSeedDefaultLensRestoresFromAnyState(t *testing.T) {
	ensureRegisteredEnabled := func(t *testing.T, s *store.Store) {
		t.Helper()
		if err := seedDefaultLens(s); err != nil {
			t.Fatalf("seedDefaultLens: %v", err)
		}
		if !slices.Contains(s.RegisteredLenses(), store.LensDefault) {
			t.Fatal("default must be registered after load-default")
		}
		if !slices.Contains(s.LoadConfig().EnabledLenses, store.LensDefault) {
			t.Fatal("default must be ENABLED after load-default (the disabled-restore bug)")
		}
	}

	// (a) DEREGISTERED → restore.
	sa := openSeedTestStore(t, filepath.Join(t.TempDir(), "a"))
	if err := sa.DeregisterLens(store.LensDefault); err != nil {
		t.Fatalf("deregister: %v", err)
	}
	ensureRegisteredEnabled(t, sa)

	// (b) DISABLED (still registered) → re-enable. This is the case the old guard missed.
	sb := openSeedTestStore(t, filepath.Join(t.TempDir(), "b"))
	if err := sb.DisableLens(store.LensDefault); err != nil {
		t.Fatalf("disable: %v", err)
	}
	if slices.Contains(sb.LoadConfig().EnabledLenses, store.LensDefault) {
		t.Fatal("precondition: default should be disabled before restore")
	}
	ensureRegisteredEnabled(t, sb)

	// (c) ALREADY present + enabled → harmless idempotent no-op.
	sc := openSeedTestStore(t, filepath.Join(t.TempDir(), "c"))
	ensureRegisteredEnabled(t, sc)
}
