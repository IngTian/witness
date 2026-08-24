package store

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

// seedRow writes one raw row so an export has content + a live WAL to consolidate.
func seedRow(t *testing.T, st *Store) {
	t.Helper()
	if err := st.AppendRaw(RawRecord{
		TS: "2026-01-01T00:00:00Z", Session: "s1", Seq: 1, Role: "user", Text: "hello",
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
}

// TestExportProducesConsistentSnapshot is the core contract: export writes a
// single plain .db (no -wal/-shm), it is a valid witness.db that opens and
// contains the data, and it can be produced while the worker would be writing.
func TestExportProducesConsistentSnapshot(t *testing.T) {
	t.Setenv("WITNESS_HOME", t.TempDir())
	st, err := Open()
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	seedRow(t, st)

	dst := filepath.Join(t.TempDir(), "snap.db")
	if err := st.Export(dst, false); err != nil {
		t.Fatalf("export: %v", err)
	}

	// Single consistent file: no WAL/SHM sidecars beside the snapshot.
	for _, sfx := range []string{"-wal", "-shm"} {
		if _, err := os.Stat(dst + sfx); err == nil {
			t.Errorf("snapshot has %s sidecar; not a clean single-file export", sfx)
		}
	}
	// Snapshot is 0600 (private growth data).
	if fi, err := os.Stat(dst); err != nil {
		t.Fatalf("stat snapshot: %v", err)
	} else if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("snapshot perms = %o, want 600", perm)
	}

	// The snapshot is a valid witness.db containing the seeded row.
	db, err := sql.Open("sqlite", dst)
	if err != nil {
		t.Fatalf("open snapshot: %v", err)
	}
	defer db.Close()
	var n int
	if err := db.QueryRow("SELECT count(*) FROM raw WHERE session='s1'").Scan(&n); err != nil {
		t.Fatalf("query snapshot: %v", err)
	}
	if n != 1 {
		t.Errorf("snapshot row count = %d, want 1 (data missing from export)", n)
	}
}

// TestExportRefusesOverwriteWithoutForce guards against silently clobbering a
// prior backup; --force must remove-then-write.
func TestExportRefusesOverwriteWithoutForce(t *testing.T) {
	t.Setenv("WITNESS_HOME", t.TempDir())
	st, err := Open()
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	seedRow(t, st)

	dst := filepath.Join(t.TempDir(), "snap.db")
	if err := st.Export(dst, false); err != nil {
		t.Fatalf("first export: %v", err)
	}
	// Second export without force must fail and leave the existing file intact.
	if err := st.Export(dst, false); err == nil {
		t.Fatal("export over an existing file without --force should error")
	}
	// With force it succeeds.
	if err := st.Export(dst, true); err != nil {
		t.Fatalf("export --force over existing: %v", err)
	}
}

// TestExportEmptyPath rejects a blank destination.
func TestExportEmptyPath(t *testing.T) {
	t.Setenv("WITNESS_HOME", t.TempDir())
	st, err := Open()
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.Export("", false); err == nil {
		t.Fatal("export with empty path should error")
	}
}

// TestExportRejectsDirectoryDestination: a directory is the --all shape, so the
// database-only path must say that instead of failing deep in os.Remove ("directory
// not empty"), which explains nothing.
func TestExportRejectsDirectoryDestination(t *testing.T) {
	st := tempStore(t)
	dir := t.TempDir()
	err := st.Export(dir, true) // even WITH force: a directory is never a snapshot file
	if err == nil {
		t.Fatal("export to a directory should error")
	}
	if !strings.Contains(err.Error(), "--all") {
		t.Errorf("error should point at --all, got: %v", err)
	}
}

// seedFullArchive fills a store with one of EVERY archive piece — a raw turn (DB), a
// registered+enabled lens (config.toml + lenses/), and both profile files — plus the
// things a complete export must NOT carry (log, OpenCode runtime, lock files). It
// returns the lens's mining prompt so a caller can assert the exact bytes survived.
func seedFullArchive(t *testing.T, st *Store) (extract string) {
	t.Helper()
	seedRow(t, st)
	extract = "MINE: the authored prompt that exists nowhere in the database"
	if err := st.RegisterLens("math", writeLensSrcDir(t, "math", extract, "REVIEW: fold it")); err != nil {
		t.Fatalf("RegisterLens: %v", err)
	}
	st.EnableLens("math") // writes config.toml (and .config-write.lock)
	// A symlink planted in the registry, pointing OUTSIDE the archive. A backup must not
	// follow it (that would pull in arbitrary files, or loop). Best-effort: unprivileged
	// Windows can't create one, and the assertions degrade harmlessly if it's absent.
	outside := filepath.Join(t.TempDir(), "outside.md")
	if err := os.WriteFile(outside, []byte("not part of the archive"), 0o600); err != nil {
		t.Fatalf("write outside file: %v", err)
	}
	_ = os.Symlink(outside, filepath.Join(st.Root, "lenses", "math", "outside.md"))
	if err := st.WriteProfile("math", "# math\nnarrative\n"); err != nil {
		t.Fatalf("WriteProfile: %v", err)
	}
	if err := st.WriteProfile(ProfileUnified, "# hand-edited portrait\n"); err != nil {
		t.Fatalf("WriteProfile unified: %v", err)
	}
	// Deliberately-excluded residents of the same directory.
	if err := os.WriteFile(st.LogPath(), []byte(`{"msg":"noise"}`+"\n"), 0o600); err != nil {
		t.Fatalf("write log: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(st.Root, "runtime", "xdg"), 0o700); err != nil {
		t.Fatalf("mkdir runtime: %v", err)
	}
	if unlock, ok := st.WorkerLock(); ok {
		unlock()
	}
	if unlock, ok := st.ImportLock("opencode"); ok {
		unlock()
	}
	return extract
}

// TestExportAllCapturesEverythingBesideTheDatabase is the contract of --all: the three
// pieces that are NOT in the database come along. lenses/ is the load-bearing one —
// prompt text has no other copy, so an authored lens is unrecoverable without it.
func TestExportAllCapturesEverythingBesideTheDatabase(t *testing.T) {
	st := tempStore(t)
	extract := seedFullArchive(t, st)

	dst := filepath.Join(t.TempDir(), "archive")
	items, err := st.ExportAll(dst, false)
	if err != nil {
		t.Fatalf("ExportAll: %v", err)
	}

	// Present, with the AUTHORED prompt bytes intact.
	for _, rel := range []string{
		"witness.db",
		"config.toml",
		filepath.Join("lenses", "math", "lens.json"),
		filepath.Join("lenses", "math", "extract.md"),
		filepath.Join("lenses", "math", "review.md"),
		filepath.Join("profile", "math.md"),
		filepath.Join("profile", "unified.md"),
	} {
		if _, err := os.Stat(filepath.Join(dst, rel)); err != nil {
			t.Errorf("snapshot is missing %s: %v", rel, err)
		}
	}
	got, err := os.ReadFile(filepath.Join(dst, "lenses", "math", "extract.md"))
	if err != nil {
		t.Fatalf("read exported prompt: %v", err)
	}
	if string(got) != extract {
		t.Errorf("exported mining prompt = %q, want %q", got, extract)
	}

	// Deliberately absent: WAL sidecars (folded in by VACUUM INTO), diagnostics, the
	// disposable OpenCode runtime, process locks, and the planted symlink (following it
	// would make a backup reach outside the archive).
	for _, rel := range []string{"witness.db-wal", "witness.db-shm", "witness.log", "runtime", ".worker.lock", ".opencode-sync.lock",
		filepath.Join("lenses", "math", "outside.md")} {
		if _, err := os.Stat(filepath.Join(dst, rel)); err == nil {
			t.Errorf("snapshot should not contain %s", rel)
		}
	}
	// No staging residue left beside the snapshot.
	for _, sfx := range []string{".witness-tmp", ".witness-bak"} {
		if _, err := os.Stat(dst + sfx); err == nil {
			t.Errorf("staging dir %s left behind", dst+sfx)
		}
	}

	// The report names what was actually captured, so a scripted backup can assert on it.
	byName := map[string]ExportItem{}
	for _, it := range items {
		byName[it.Name] = it
	}
	if it, ok := byName["lenses"]; !ok || !it.Dir || it.Files != 3 {
		t.Errorf("items should report lenses/ as a dir with 3 files, got %+v", byName)
	}
	if it, ok := byName["profile"]; !ok || it.Files != 2 {
		t.Errorf("items should report profile/ with 2 files, got %+v", byName)
	}
}

// TestExportAllSnapshotIsARunnableDataDir is the property that makes restore free and
// is the real proof of "everything": the exported directory has the same layout as the
// live data root, so witness can be pointed straight at it and finds its database, its
// enabled lenses, the authored lens definition, and the hand-edited portrait. Asserting
// the OUTCOME (a working archive) rather than a file list means a future piece added to
// the data root can't pass this by being copied to the wrong place.
func TestExportAllSnapshotIsARunnableDataDir(t *testing.T) {
	st := tempStore(t)
	seedFullArchive(t, st)

	dst := filepath.Join(t.TempDir(), "archive")
	if _, err := st.ExportAll(dst, false); err != nil {
		t.Fatalf("ExportAll: %v", err)
	}

	// Re-open witness against the snapshot itself — this is the documented restore path.
	t.Setenv("WITNESS_HOME", dst)
	restored, err := Open()
	if err != nil {
		t.Fatalf("open snapshot as a data dir: %v", err)
	}
	defer restored.Close()

	if n := restored.RawCount("s1"); n != 1 {
		t.Errorf("restored raw count = %d, want 1", n)
	}
	if got := restored.RegisteredLenses(); !slices.Contains(got, "math") {
		t.Errorf("restored registry lost the authored lens: %v", got)
	}
	if got := restored.LoadConfig().EnabledLenses; !slices.Contains(got, "math") {
		t.Errorf("restored config lost the enabled lens: %v — distillation would be silently off", got)
	}
	portrait, ok, err := restored.ReadProfile(ProfileUnified)
	if err != nil || !ok {
		t.Fatalf("restored unified portrait: ok=%v err=%v", ok, err)
	}
	if portrait != "# hand-edited portrait\n" {
		t.Errorf("restored portrait = %q; a hand-edited unified.md has no other copy", portrait)
	}
}

// TestExportAllRefusesLiveDataDir: with --force the swap would move the user's real
// archive aside and then delete it — WAL, log and runtime included — while a worker may
// be mid-write. Refuse, and leave the live root untouched.
func TestExportAllRefusesLiveDataDir(t *testing.T) {
	st := tempStore(t)
	seedFullArchive(t, st)

	if _, err := st.ExportAll(st.Root, true); err == nil {
		t.Fatal("exporting onto the live data dir should error")
	}
	// Also via a different spelling of the same directory (identity, not string compare).
	if _, err := st.ExportAll(filepath.Join(st.Root, "..", filepath.Base(st.Root)), true); err == nil {
		t.Error("exporting onto the live data dir by another path should error")
	}
	for _, rel := range []string{"witness.db", "config.toml", filepath.Join("lenses", "math", "extract.md")} {
		if _, err := os.Stat(filepath.Join(st.Root, rel)); err != nil {
			t.Errorf("live archive lost %s: %v", rel, err)
		}
	}
}

// TestExportAllForceSemantics: refuse to clobber a prior snapshot without --force, and
// with it produce a fresh one that reflects the CURRENT archive.
func TestExportAllForceSemantics(t *testing.T) {
	st := tempStore(t)
	seedFullArchive(t, st)
	dst := filepath.Join(t.TempDir(), "archive")

	if _, err := st.ExportAll(dst, false); err != nil {
		t.Fatalf("first export: %v", err)
	}
	if _, err := st.ExportAll(dst, false); err == nil {
		t.Fatal("second export without --force should error")
	}
	// A lens registered after the first export must appear in a forced re-export.
	if err := st.RegisterLens("later", writeLensSrcDir(t, "later", "second prompt", "")); err != nil {
		t.Fatalf("RegisterLens: %v", err)
	}
	if _, err := st.ExportAll(dst, true); err != nil {
		t.Fatalf("export --force: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dst, "lenses", "later", "extract.md")); err != nil {
		t.Errorf("forced re-export is stale — missing the newly registered lens: %v", err)
	}
}

// TestExportAllKeepsPreviousSnapshotWhenExportFails: a backup command must not destroy
// yesterday's good backup to produce today's broken one. Everything stages into a
// sibling dir and the old snapshot is moved aside, not deleted, until the swap lands —
// so a cron `export --all --force` that dies halfway leaves the previous one intact.
func TestExportAllKeepsPreviousSnapshotWhenExportFails(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("permission-based failure injection does not apply on Windows")
	}
	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory permissions, so the copy cannot be made to fail")
	}
	st := tempStore(t)
	seedFullArchive(t, st)
	dst := filepath.Join(t.TempDir(), "archive")
	if _, err := st.ExportAll(dst, false); err != nil {
		t.Fatalf("first export: %v", err)
	}

	// Make the lens copy fail midway through the NEXT export.
	lensDir := filepath.Join(st.Root, "lenses")
	if err := os.Chmod(lensDir, 0); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(lensDir, 0o700) }) // else TempDir cleanup fails
	if _, err := st.ExportAll(dst, true); err == nil {
		t.Fatal("export should fail when the lens registry is unreadable")
	}

	// The previous snapshot survived, whole.
	for _, rel := range []string{"witness.db", "config.toml", filepath.Join("lenses", "math", "extract.md"), filepath.Join("profile", "unified.md")} {
		if _, err := os.Stat(filepath.Join(dst, rel)); err != nil {
			t.Errorf("failed export destroyed the previous snapshot's %s: %v", rel, err)
		}
	}
	// And left no half-built staging dir masquerading as a backup.
	for _, sfx := range []string{".witness-tmp", ".witness-bak"} {
		if _, err := os.Stat(dst + sfx); err == nil {
			t.Errorf("staging dir %s left behind after a failed export", dst+sfx)
		}
	}
}

// TestExportAllRestoresPreviousSnapshotWhenSwapFails covers the other half of "don't
// lose yesterday's backup": the staging test proves a failure BEFORE the swap is
// harmless, and this one proves a failure DURING it is too — the previous snapshot is
// moved aside, not deleted, so it can be put back. Injected through renameForExport
// because a same-directory rename into a directory just written to cannot be made to
// fail from outside the process.
func TestExportAllRestoresPreviousSnapshotWhenSwapFails(t *testing.T) {
	st := tempStore(t)
	seedFullArchive(t, st)
	dst := filepath.Join(t.TempDir(), "archive")
	if _, err := st.ExportAll(dst, false); err != nil {
		t.Fatalf("first export: %v", err)
	}

	orig := renameForExport
	t.Cleanup(func() { renameForExport = orig })
	// Keyed on the SOURCE so exactly one rename fails: staging -> destination. The
	// move-aside (dst -> .witness-bak) and the recovery (.witness-bak -> dst) must both
	// still work, or the test would be injecting a broken filesystem rather than the one
	// failure whose handling it is checking.
	renameForExport = func(from, to string) error {
		if from == dst+".witness-tmp" {
			return fmt.Errorf("injected swap failure")
		}
		return orig(from, to)
	}
	if _, err := st.ExportAll(dst, true); err == nil {
		t.Fatal("export should fail when the swap-into-place fails")
	}
	renameForExport = orig

	// The previous snapshot is back where it was, intact.
	for _, rel := range []string{"witness.db", "config.toml", filepath.Join("lenses", "math", "extract.md"), filepath.Join("profile", "unified.md")} {
		if _, err := os.Stat(filepath.Join(dst, rel)); err != nil {
			t.Errorf("a failed swap lost the previous snapshot's %s: %v", rel, err)
		}
	}
	if _, err := os.Stat(dst + ".witness-tmp"); err == nil {
		t.Error("staging dir left behind after a failed swap")
	}
}

// TestExportAllClassifiesEveryRootEntry is the census guard. The defect --all exists to
// fix was a SILENT omission: three pieces of the archive sat next to the database and
// no one noticed they weren't in the backup. So every entry witness creates in its data
// root must be classified on purpose — exported, or skipped with a reason — and adding
// a new one without deciding fails here rather than quietly falling out of backups.
func TestExportAllClassifiesEveryRootEntry(t *testing.T) {
	st := tempStore(t)
	seedFullArchive(t, st)

	entries, err := os.ReadDir(st.Root)
	if err != nil {
		t.Fatalf("read data root: %v", err)
	}
	seen := map[string]bool{}
	for _, e := range entries {
		name := e.Name()
		seen[name] = true
		exported, skipped := classifyRootEntry(name)
		if !exported && !skipped {
			t.Errorf("data-root entry %q is neither exported nor skipped: add it to exportInclude "+
				"(if it is archive data) or exportSkip (with the reason it isn't), else it will "+
				"silently vanish from every backup", name)
		}
	}
	// The include list must name things that actually exist, so a rename can't leave it
	// pointing at nothing while still reading like a complete backup.
	for _, name := range exportInclude {
		if !seen[name] {
			t.Errorf("exportInclude names %q, which a fully-seeded archive does not contain — renamed or removed?", name)
		}
	}
}
