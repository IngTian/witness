package commands

import (
	"path/filepath"
	"slices"
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

	// Simulate a pre-1a archive: real L0 + a distillation watermark under "default",
	// which is exactly what HasLegacyDefaultData keys on. (A fresh Open already ran the
	// hook once and found no legacy data — this seeds the legacy signal for the re-open.)
	if err := s.AppendRaw(store.RawRecord{Session: "s", Seq: 0, Role: "user", Text: "hi"}); err != nil {
		t.Fatalf("AppendRaw: %v", err)
	}
	if err := s.MarkDistilled("s", store.LensDefault, 1); err != nil {
		t.Fatalf("MarkDistilled: %v", err)
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
