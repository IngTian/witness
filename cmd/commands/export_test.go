package commands

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/IngTian/witness/internal/store"
)

// seedArchiveForExport lays down an archive with an authored lens and a hand-edited
// portrait — the two pieces that live OUTSIDE the database — and returns the data dir.
func seedArchiveForExport(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("WITNESS_HOME", home)
	t.Setenv("WITNESS_PROMPTS", filepath.Join("..", "..", "prompts"))
	st, err := store.Open()
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "extract.md"), []byte("authored mining prompt"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := st.RegisterLens("corpus", src); err != nil {
		t.Fatalf("RegisterLens: %v", err)
	}
	st.EnableLens("corpus")
	if err := st.WriteProfile(store.ProfileUnified, "# portrait\n"); err != nil {
		t.Fatalf("WriteProfile: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	return home
}

// TestExportCarriesAllFlag is a surface guard: --all is what the README tells people to
// use for a backup, so losing the flag must break a test rather than a user's restore.
func TestExportCarriesAllFlag(t *testing.T) {
	c := newExportCmd()
	for _, name := range []string{"all", "force", "json"} {
		if c.Flags().Lookup(name) == nil {
			t.Errorf("export must have a --%s flag", name)
		}
	}
}

// TestExportAllRejectsDatabaseSuffix: `export --all backup.db` would create a DIRECTORY
// named backup.db, which reads as a corrupt database to every tool the user reaches for
// next. Reject it before opening the store, so the mistake costs nothing.
func TestExportAllRejectsDatabaseSuffix(t *testing.T) {
	err := cmdExport(filepath.Join(t.TempDir(), "backup.db"), false, true, false)
	if err == nil {
		t.Fatal("export --all with a .db path should error")
	}
	if !strings.Contains(err.Error(), "DIRECTORY") {
		t.Errorf("error should explain --all writes a directory, got: %v", err)
	}
}

// TestExportAllCommandWritesCompleteArchive is the end-to-end shape: the command writes
// the database plus the three non-database pieces, and tells the user how to restore
// (there is no restore command — the snapshot IS a data dir).
func TestExportAllCommandWritesCompleteArchive(t *testing.T) {
	seedArchiveForExport(t)
	dst := filepath.Join(t.TempDir(), "archive")

	out, err := captureStdoutErr(t, func() error { return cmdExport(dst, false, true, false) })
	if err != nil {
		t.Fatalf("export --all: %v", err)
	}
	for _, rel := range []string{
		"witness.db",
		"config.toml",
		filepath.Join("lenses", "corpus", "extract.md"),
		filepath.Join("profile", "unified.md"),
	} {
		if _, err := os.Stat(filepath.Join(dst, rel)); err != nil {
			t.Errorf("complete export is missing %s: %v", rel, err)
		}
	}
	if !strings.Contains(out, "WITNESS_HOME") {
		t.Errorf("output should tell the user how to restore, got:\n%s", out)
	}
}

// TestExportAllJSONReportsWhatWasCaptured: a scripted/cron backup needs to verify its
// own result, so --json lists the items actually written (and stays ANSI-free).
func TestExportAllJSONReportsWhatWasCaptured(t *testing.T) {
	seedArchiveForExport(t)
	dst := filepath.Join(t.TempDir(), "archive")

	old := useColor
	useColor = true // prove decoration is gated off for --json even on a "TTY"
	defer func() { useColor = old }()

	out, err := captureStdoutErr(t, func() error { return cmdExport(dst, false, true, true) })
	if err != nil {
		t.Fatalf("export --all --json: %v", err)
	}
	if strings.Contains(out, "\x1b") {
		t.Errorf("--json must not contain ANSI escapes: %q", out)
	}
	var got exportAllJSON
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("--json output is not valid JSON (%v): %s", err, out)
	}
	if !got.All || got.Exported != dst {
		t.Errorf("report should name the destination and the mode, got %+v", got)
	}
	names := map[string]int{}
	for _, it := range got.Items {
		names[it.Name] = it.Files
	}
	if names["witness.db"] != 1 {
		t.Errorf("report should include the database, got %v", names)
	}
	if names["lenses"] < 1 {
		t.Errorf("report should include lenses/ with the authored prompt, got %v", names)
	}
	if names["profile"] < 1 {
		t.Errorf("report should include profile/, got %v", names)
	}
}

// TestExportDatabaseOnlyPointsAtAll: the plain form is still correct, but it is a PARTIAL
// backup — the README used to present it as "back up the archive", which is how three
// pieces went unbacked-up unnoticed. Its output must say so.
func TestExportDatabaseOnlyPointsAtAll(t *testing.T) {
	seedArchiveForExport(t)
	dst := filepath.Join(t.TempDir(), "snap.db")

	out, err := captureStdoutErr(t, func() error { return cmdExport(dst, false, false, false) })
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	if !strings.Contains(out, "--all") {
		t.Errorf("database-only output must mention --all, got:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(dst)); err != nil {
		t.Errorf("snapshot not written: %v", err)
	}
}
