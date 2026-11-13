package commands

import (
	"database/sql"
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/IngTian/witness/internal/platform/opencode"
	"github.com/IngTian/witness/internal/store"
)

// TestDrainStartsWithAnOpenCodeImportWhenWired: a wired OpenCode (plugin file exists)
// is imported incrementally at every drain start, so Claude-kicked drains pick up sessions
// a silently-rejected plugin missed.
func TestDrainStartsWithAnOpenCodeImportWhenWired(t *testing.T) {
	t.Setenv("WITNESS_HOME", filepath.Join(t.TempDir(), "witness"))
	t.Setenv("WITNESS_NO_DEFAULT_LENS", "1")
	configDir := t.TempDir()
	t.Setenv("OPENCODE_CONFIG_DIR", configDir)
	t.Setenv("WITNESS_OPENCODE_DB", seedOpenCodeV2DB(t, 5000))

	// Write the plugin file to wire OpenCode.
	pluginPath := filepath.Join(configDir, "plugins", opencode.PluginFileName)
	if err := os.MkdirAll(filepath.Dir(pluginPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pluginPath, []byte("// wired"), 0o644); err != nil {
		t.Fatal(err)
	}

	ran, err := runWorker(false)
	if err != nil {
		t.Fatalf("runWorker: %v", err)
	}
	if !ran {
		t.Fatal("worker should have run")
	}

	st, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	if got := st.RawCount("opencode:ses_fixture"); got != 2 {
		t.Fatalf("RawCount(opencode:ses_fixture) = %d, want 2 (user+assistant)", got)
	}
}

// TestDrainDoesNotImportAnUnwiredOpenCode: an unwired OpenCode (no plugin file, no prior
// import) is never imported at drain start — the user never wired it, so witness must not
// capture an editor they did not opt into.
func TestDrainDoesNotImportAnUnwiredOpenCode(t *testing.T) {
	t.Setenv("WITNESS_HOME", filepath.Join(t.TempDir(), "witness"))
	t.Setenv("WITNESS_NO_DEFAULT_LENS", "1")
	t.Setenv("OPENCODE_CONFIG_DIR", t.TempDir()) // empty, no plugin
	t.Setenv("WITNESS_OPENCODE_DB", seedOpenCodeV2DB(t, 5000))

	ran, err := runWorker(false)
	if err != nil {
		t.Fatalf("runWorker: %v", err)
	}
	if !ran {
		t.Fatal("worker should have run")
	}

	st, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	if got := st.RawCount("opencode:ses_fixture"); got != 0 {
		t.Fatalf("unwired OpenCode must not be imported, got %d raw records", got)
	}
}

// wireOpenCodeForTest plants a plugin file in configDir, which is what makes OpenCode "wired" for
// ImportAtDrainStart and health.
func wireOpenCodeForTest(t *testing.T, configDir string) {
	t.Helper()
	pluginPath := filepath.Join(configDir, "plugins", opencode.PluginFileName)
	if err := os.MkdirAll(filepath.Dir(pluginPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pluginPath, []byte("// wired"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// drainLog runs one foreground drain and returns what it logged: runWorker points slog at the
// archive's witness.log, so the drain-start import's warnings land there.
func drainLog(t *testing.T) string {
	t.Helper()
	prev := slog.Default()
	t.Cleanup(func() { slog.SetDefault(prev) })
	ran, err := runWorker(false)
	if err != nil {
		t.Fatalf("drain must succeed whatever the import does, got err=%v", err)
	}
	if !ran {
		t.Fatal("worker should have run")
	}
	st, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	b, err := os.ReadFile(st.LogPath())
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		t.Fatal(err)
	}
	return string(b)
}

// TestDrainStartImportNeverFailsTheDrain: even when the drain-start import fails (here a v1-only
// DB, which ProbeSchema refuses), the drain proceeds with what was already captured. Import errors
// are logged but never surfaced as drain errors.
func TestDrainStartImportNeverFailsTheDrain(t *testing.T) {
	t.Setenv("WITNESS_HOME", filepath.Join(t.TempDir(), "witness"))
	t.Setenv("WITNESS_NO_DEFAULT_LENS", "1")
	configDir := t.TempDir()
	t.Setenv("OPENCODE_CONFIG_DIR", configDir)

	// A real v1-only DB (a v1 table, no session_v2): a fake header would fail as "file is not a
	// database" before ProbeSchema ever ran.
	v1Path := filepath.Join(t.TempDir(), "v1.db")
	db, err := sql.Open("sqlite", v1Path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE session (id TEXT PRIMARY KEY)`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	t.Setenv("WITNESS_OPENCODE_DB", v1Path)
	wireOpenCodeForTest(t, configDir)

	// The drain must succeed even though the import fails, and the failure is logged, naming v1
	logged := drainLog(t)
	if !strings.Contains(logged, "drain-start import failed") || !strings.Contains(logged, "OpenCode v1") {
		t.Fatalf("witness.log does not record the refused v1 import; got:\n%s", logged)
	}
}

// Wired before OpenCode was first used: no DB yet is nothing to import, not a warning on every drain.
func TestDrainStartImportIsQuietWhenOpenCodeHasNoDatabaseYet(t *testing.T) {
	t.Setenv("WITNESS_HOME", filepath.Join(t.TempDir(), "witness"))
	t.Setenv("WITNESS_NO_DEFAULT_LENS", "1")
	configDir := t.TempDir()
	t.Setenv("OPENCODE_CONFIG_DIR", configDir)
	t.Setenv("WITNESS_OPENCODE_DB", filepath.Join(t.TempDir(), "opencode.db"))
	wireOpenCodeForTest(t, configDir)

	if logged := drainLog(t); strings.Contains(logged, "drain-start import failed") {
		t.Fatalf("a missing OpenCode DB was logged as a failed import; got:\n%s", logged)
	}
}

// Unwire is the consent switch: after `witness unwire opencode`, no drain imports OpenCode again,
// even though an earlier import had recorded what it saw, and status no longer checks it.
func TestUnwireStopsOpenCodeCapture(t *testing.T) {
	t.Setenv("WITNESS_HOME", filepath.Join(t.TempDir(), "witness"))
	t.Setenv("WITNESS_NO_DEFAULT_LENS", "1")
	configDir := t.TempDir()
	t.Setenv("OPENCODE_CONFIG_DIR", configDir)
	t.Setenv("OPENCODE_CONFIG", "")
	dbPath := seedOpenCodeV2DB(t, 5000)
	t.Setenv("WITNESS_OPENCODE_DB", dbPath)
	wireOpenCodeForTest(t, configDir)

	if _, err := runWorker(false); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	if got := st.RawCount("opencode:ses_fixture"); got != 2 {
		st.Close()
		t.Fatalf("precondition: wired drain imported %d records, want 2", got)
	}
	st.Close()

	var unwireErr error
	out := captureStdout(t, func() { unwireErr = cmdUninstall([]string{"opencode"}) })
	if unwireErr != nil {
		t.Fatal(unwireErr)
	}
	if !strings.Contains(out, "stopped") || !strings.Contains(out, "drains no longer import OpenCode") {
		t.Errorf("unwire output does not say capture stopped:\n%s", out)
	}

	addOpenCodeSession(t, dbPath, "ses_after", 9000)
	if _, err := runWorker(false); err != nil {
		t.Fatal(err)
	}
	st, err = store.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if got := st.RawCount("opencode:ses_after"); got != 0 {
		t.Fatalf("a drain after unwire imported %d records of a new OpenCode session, want 0", got)
	}
	if checks := openCodeHealth(st, true); checks != nil {
		t.Errorf("status still checks an unwired OpenCode: %+v", checks)
	}
}
