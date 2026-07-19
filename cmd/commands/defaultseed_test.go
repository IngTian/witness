package commands

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/IngTian/witness/internal/store"
)

// openSeedTestStore opens a store in a fresh WITNESS_HOME with the bundled prompts
// reachable (so lens.DefaultSeedDir resolves during the seed hook). Returns the store
// and the home dir so a test can re-open the SAME archive to exercise the one-shot.
func openSeedTestStore(t *testing.T, home string) *store.Store {
	t.Helper()
	t.Setenv("WITNESS_HOME", home)
	t.Setenv("WITNESS_PROMPTS", filepath.Join("..", "..", "prompts"))
	s, err := store.Open()
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// TestDefaultSeedMigratesPre1aArchive is the #44 slice-1a migration guard: an archive
// that distilled under the OLD always-on default lens (a progress row for lens
// "default", but no lenses/default/ registry dir) must, on the next Open, have default
// retro-registered + enabled — so an existing install keeps working after default
// loses its built-in status. Then it must be idempotent (a second Open does nothing new).
func TestDefaultSeedMigratesPre1aArchive(t *testing.T) {
	home := filepath.Join(t.TempDir(), "witness")
	s := openSeedTestStore(t, home)

	// Simulate a genuine PRE-1a archive: real L0 + a distillation watermark under
	// "default" (what HasLegacyDefaultData keys on), and NO migration flag — the flag
	// didn't exist before this feature. The first openSeedTestStore already ran the hook
	// on the then-empty archive and stamped the flag "n/a"; clear it so the re-open sees
	// the true pre-1a state (legacy data present, never migrated).
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

	// Re-open the SAME archive → the seed hook sees legacy default data + no registry
	// entry → migrates.
	s2 := openSeedTestStore(t, home)
	if !slices.Contains(s2.RegisteredLenses(), store.LensDefault) {
		t.Fatal("migration must retro-register the default lens into the registry")
	}
	if !slices.Contains(s2.LoadConfig().EnabledLenses, store.LensDefault) {
		t.Fatal("migration must ENABLE the default lens so the install keeps distilling it")
	}
	// activeLenses must now include default, loaded from the registry.
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

// TestDeregisteredDefaultStaysGone is the regression guard for the adversarial
// finding that the migration RESURRECTED a deregistered default forever (the two gates
// disagreed: trigger keyed on ever-present progress rows, idempotency on registry
// presence). The one-shot meta flag now makes the migration truly one-shot: after it
// runs once, deregistering + disabling default must STICK across re-opens — the "run
// any set of lenses, including none" promise. The legacy default-progress rows persist
// (deregister doesn't clear them), so this specifically proves the gate is the FLAG,
// not those rows.
func TestDeregisteredDefaultStaysGone(t *testing.T) {
	home := filepath.Join(t.TempDir(), "witness")
	// Migrate a pre-1a archive (as above): legacy data + cleared flag → Open seeds default.
	s := openSeedTestStore(t, home)
	if err := s.AppendRaw(store.RawRecord{Session: "s", Seq: 0, Role: "user", Text: "hi"}); err != nil {
		t.Fatalf("AppendRaw: %v", err)
	}
	if err := s.MarkDistilled("s", store.LensDefault, 1); err != nil {
		t.Fatalf("MarkDistilled: %v", err)
	}
	if err := s.SetMetaString(defaultMigratedKey, ""); err != nil {
		t.Fatalf("clear flag: %v", err)
	}
	_ = s.Close()
	s2 := openSeedTestStore(t, home) // migrates → default registered+enabled
	if !slices.Contains(s2.RegisteredLenses(), store.LensDefault) {
		t.Fatal("precondition: migration should have seeded default")
	}

	// The user removes default entirely — the exact "I don't want this lens" action.
	if err := s2.DisableLens(store.LensDefault); err != nil {
		t.Fatalf("DisableLens: %v", err)
	}
	if err := s2.DeregisterLens(store.LensDefault); err != nil {
		t.Fatalf("DeregisterLens: %v", err)
	}
	// The legacy signal that used to (wrongly) trigger re-seeding still exists...
	if !s2.HasLegacyDefaultData() {
		t.Fatal("precondition: legacy default progress rows should persist (deregister doesn't clear them)")
	}
	_ = s2.Close()

	// ...but across TWO more Opens, default must STAY gone (flag gates the one-shot).
	s3 := openSeedTestStore(t, home)
	if slices.Contains(s3.RegisteredLenses(), store.LensDefault) {
		t.Fatal("BUG: deregistered default was resurrected in the registry on re-open")
	}
	if slices.Contains(s3.LoadConfig().EnabledLenses, store.LensDefault) {
		t.Fatal("BUG: deregistered default was re-enabled on re-open")
	}
	_ = s3.Close()
	s4 := openSeedTestStore(t, home)
	if slices.Contains(s4.RegisteredLenses(), store.LensDefault) {
		t.Fatal("BUG: deregistered default resurrected on the second re-open")
	}
}

// TestRebuildDisabledDefaultFailsFast is the regression guard for the adversarial
// data-loss finding: `lens rebuild default` on a DISABLED default used to bypass the
// registered+enabled precondition (an `if name != LensDefault` special-case), so it
// DeleteLensData'd default's observations+facets and then no-op'd the re-mine (the drain
// excludes inactive lenses) — irreversible loss. The special-case is gone; default is now
// subject to the same guard as any lens, so rebuild fails fast BEFORE dropping anything.
func TestRebuildDisabledDefaultFailsFast(t *testing.T) {
	home := filepath.Join(t.TempDir(), "witness")
	s := openSeedTestStore(t, home)
	// Register+enable default, then seed L1/L2 + a watermark under it.
	if err := seedDefaultLens(s); err != nil {
		t.Fatalf("seedDefaultLens: %v", err)
	}
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

	// rebuild must FAIL FAST (not enabled) and drop NOTHING.
	err := lensBackfill(s, store.LensDefault, true)
	if err == nil {
		t.Fatal("rebuild of a DISABLED default must fail fast, not proceed to delete data")
	}
	if !strings.Contains(err.Error(), "not enabled") {
		t.Fatalf("expected an 'enable it first' error, got: %v", err)
	}
	obsAfter, _ := s.ReadObservations("")
	if len(obsAfter) != len(obsBefore) || len(obsAfter) == 0 {
		t.Fatalf("rebuild must NOT have dropped observations: before=%d after=%d", len(obsBefore), len(obsAfter))
	}
}

// TestSeedDefaultLensScaffoldsFreshArchive covers the tool-install scaffold path
// (slice-1a step 6): `witness install` calls seedDefaultLens on a fresh archive so a
// new personal user starts with the default lens registered + enabled (default no
// longer being always-on). Direct call — full cmdInstall needs the claude/opencode
// binaries, but this is the exact seeding step it performs.
func TestSeedDefaultLensScaffoldsFreshArchive(t *testing.T) {
	s := openSeedTestStore(t, filepath.Join(t.TempDir(), "fresh"))
	if slices.Contains(s.RegisteredLenses(), store.LensDefault) {
		t.Fatal("precondition: fresh archive has no default lens until scaffolded")
	}
	if err := seedDefaultLens(s); err != nil {
		t.Fatalf("seedDefaultLens: %v", err)
	}
	if !slices.Contains(s.RegisteredLenses(), store.LensDefault) {
		t.Fatal("scaffold must register the default lens")
	}
	if !slices.Contains(s.LoadConfig().EnabledLenses, store.LensDefault) {
		t.Fatal("scaffold must enable the default lens")
	}
	// The seeded lens carries the person dimensions from the bundled prompts/default/lens.json.
	act, err := activeLenses(s)
	if err != nil || len(act) != 1 || act[0].Name != store.LensDefault {
		t.Fatalf("activeLenses after scaffold = %v (err %v), want just default", act, err)
	}
	if len(act[0].Dimensions) == 0 {
		t.Fatal("scaffolded default must carry its dimensions from the seeded lens.json")
	}
}

// TestSeedDefaultLensRestoresFromAnyState guards the "restore default by re-running
// install" path (the user's natural gesture). `install` calls seedDefaultLens, which must
// leave default registered AND enabled from ALL three states a user can reach — including
// the DISABLED case, which the old install guard (skip-if-registered) silently no-op'd.
func TestSeedDefaultLensRestoresFromAnyState(t *testing.T) {
	ensureRegisteredEnabled := func(t *testing.T, s *store.Store) {
		t.Helper()
		if err := seedDefaultLens(s); err != nil {
			t.Fatalf("seedDefaultLens: %v", err)
		}
		if !slices.Contains(s.RegisteredLenses(), store.LensDefault) {
			t.Fatal("default must be registered after seed")
		}
		if !slices.Contains(s.LoadConfig().EnabledLenses, store.LensDefault) {
			t.Fatal("default must be ENABLED after seed (the disabled-restore bug)")
		}
	}

	// (a) DEREGISTERED → restore.
	sa := openSeedTestStore(t, filepath.Join(t.TempDir(), "a"))
	_ = seedDefaultLens(sa)
	if err := sa.DeregisterLens(store.LensDefault); err != nil {
		t.Fatalf("deregister: %v", err)
	}
	ensureRegisteredEnabled(t, sa)

	// (b) DISABLED (still registered) → re-enable. This is the case the old guard missed.
	sb := openSeedTestStore(t, filepath.Join(t.TempDir(), "b"))
	_ = seedDefaultLens(sb)
	if err := sb.DisableLens(store.LensDefault); err != nil {
		t.Fatalf("disable: %v", err)
	}
	if slices.Contains(sb.LoadConfig().EnabledLenses, store.LensDefault) {
		t.Fatal("precondition: default should be disabled before restore")
	}
	ensureRegisteredEnabled(t, sb)

	// (c) ALREADY present + enabled → harmless idempotent no-op.
	sc := openSeedTestStore(t, filepath.Join(t.TempDir(), "c"))
	_ = seedDefaultLens(sc)
	ensureRegisteredEnabled(t, sc)
}

// TestDefaultSeedSkipsFreshAndLibraryArchive proves the migration does NOT force
// default onto (a) a fresh archive with no distillation history, nor (b) a library-mode
// archive that only ever ran its own domain lens — both have no "default" progress row,
// so HasLegacyDefaultData is false and the hook is a no-op. (Fresh-install scaffolding
// is install/init's job, slice-1a step 6 — not the migration's.)
func TestDefaultSeedSkipsFreshAndLibraryArchive(t *testing.T) {
	// (a) Fresh: an Open on an empty home must not auto-register default.
	fresh := openSeedTestStore(t, filepath.Join(t.TempDir(), "fresh"))
	if slices.Contains(fresh.RegisteredLenses(), store.LensDefault) {
		t.Fatal("a fresh archive must NOT auto-seed default (that is install/init's consented job)")
	}

	// (b) Library-style: raw + a watermark under a NON-default lens only.
	home := filepath.Join(t.TempDir(), "lib")
	lib := openSeedTestStore(t, home)
	if err := lib.AppendRaw(store.RawRecord{Session: "s", Seq: 0, Role: "user", Text: "hi"}); err != nil {
		t.Fatalf("AppendRaw: %v", err)
	}
	if err := lib.MarkDistilled("s", "market", 1); err != nil {
		t.Fatalf("MarkDistilled: %v", err)
	}
	_ = lib.Close()
	lib2 := openSeedTestStore(t, home)
	if slices.Contains(lib2.RegisteredLenses(), store.LensDefault) {
		t.Fatal("a library archive that only ran its own lens must NOT get default forced onto it")
	}
}
