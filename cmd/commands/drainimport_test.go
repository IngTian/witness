package commands

import (
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/IngTian/witness/internal/store"
)

// drainEnv sandboxes one drain: a fresh archive with no default lens and an empty OpenCode config
// dir (nothing wired yet).
func drainEnv(t *testing.T) {
	t.Helper()
	t.Setenv("WITNESS_HOME", filepath.Join(t.TempDir(), "witness"))
	t.Setenv("WITNESS_NO_DEFAULT_LENS", "1")
	t.Setenv("OPENCODE_CONFIG_DIR", t.TempDir())
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

// rawCount is how many L0 records the archive holds for session.
func rawCount(t *testing.T, session string) int {
	t.Helper()
	st, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	return st.RawCount(session)
}

// A wired OpenCode (a plugin file) is imported incrementally at every drain start, so Claude-kicked
// drains pick up sessions a silently-rejected plugin missed. An unwired one (no plugin file, no
// prior import) never is: witness must not capture an editor the user did not opt into.
func TestDrainStartImportFollowsTheWiring(t *testing.T) {
	for _, tc := range []struct {
		name  string
		wired bool
		want  int
	}{
		{"wired", true, 2}, // user + assistant
		{"unwired", false, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			drainEnv(t)
			t.Setenv("WITNESS_OPENCODE_DB", seedOpenCodeV2DB(t, 5000))
			if tc.wired {
				plantOpenCodePlugin(t, "// wired")
			}
			drainLog(t)
			if got := rawCount(t, "opencode:ses_fixture"); got != tc.want {
				t.Fatalf("RawCount(opencode:ses_fixture) = %d, want %d", got, tc.want)
			}
		})
	}
}

// TestDrainStartImportNeverFailsTheDrain: even when the drain-start import fails (here a v1-only
// DB, which ProbeSchema refuses), the drain proceeds with what was already captured. Import errors
// are logged but never surfaced as drain errors.
func TestDrainStartImportNeverFailsTheDrain(t *testing.T) {
	drainEnv(t)
	t.Setenv("WITNESS_OPENCODE_DB", v1OnlyOpenCodeDB(t))
	plantOpenCodePlugin(t, "// wired")

	// The drain must succeed even though the import fails, and the failure is logged, naming v1
	logged := drainLog(t)
	if !strings.Contains(logged, "drain-start import failed") || !strings.Contains(logged, "OpenCode v1") {
		t.Fatalf("witness.log does not record the refused v1 import; got:\n%s", logged)
	}
}

// Wired before OpenCode was first used: no DB yet is nothing to import, not a warning on every drain.
func TestDrainStartImportIsQuietWhenOpenCodeHasNoDatabaseYet(t *testing.T) {
	drainEnv(t)
	t.Setenv("WITNESS_OPENCODE_DB", filepath.Join(t.TempDir(), "opencode.db"))
	plantOpenCodePlugin(t, "// wired")

	if logged := drainLog(t); strings.Contains(logged, "drain-start import failed") {
		t.Fatalf("a missing OpenCode DB was logged as a failed import; got:\n%s", logged)
	}
}

// Unwire must not create an archive: on a fresh machine with no witness data, the data dir
// must still not exist after unwire.
func TestUnwireNeverCreatesAnArchive(t *testing.T) {
	base := t.TempDir()
	t.Setenv("XDG_DATA_HOME", base)
	t.Setenv("WITNESS_HOME", "") // let it resolve via XDG
	t.Setenv("OPENCODE_CONFIG_DIR", t.TempDir())

	// Fresh machine: no witness dir exists yet
	witnessDir := filepath.Join(base, "witness")
	if _, err := os.Stat(witnessDir); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("precondition: %s must not exist, got err=%v", witnessDir, err)
	}

	// Unwire on a fresh machine: must not create the dir
	if err := cmdUninstall([]string{"opencode"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(witnessDir); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("after unwire: %s exists, want it still absent", witnessDir)
	}
}

// Unwire must clear the seen key from a legacy-named archive (claude-witness) so the adoption
// ladder works even when the archive predates the repo rename.
func TestUnwireClearsSeenFromLegacyNamedArchive(t *testing.T) {
	base := t.TempDir()
	legacyDir := filepath.Join(base, "claude-witness") // the legacy name
	t.Setenv("XDG_DATA_HOME", base)
	t.Setenv("WITNESS_HOME", "") // let it resolve via XDG + adoption
	t.Setenv("OPENCODE_CONFIG_DIR", t.TempDir())

	// Create a legacy-named archive with OpenCode data
	t.Setenv("WITNESS_HOME", legacyDir)
	dbPath := seedOpenCodeV2DB(t, 5000)
	t.Setenv("WITNESS_OPENCODE_DB", dbPath)
	plantOpenCodePlugin(t, "// legacy")
	drainLog(t) // import so seen is set
	st, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	st.Close()
	if _, err := os.Stat(legacyDir); err != nil {
		t.Fatalf("precondition: legacy dir must exist, got err=%v", err)
	}

	// Unwire with no WITNESS_HOME: it must adopt the legacy dir and clear seen
	t.Setenv("WITNESS_HOME", "")
	if err := cmdUninstall([]string{"opencode"}); err != nil {
		t.Fatal(err)
	}
	st, err = store.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if st.Root != legacyDir {
		t.Fatalf("after unwire: store root = %q, want %q (adopted)", st.Root, legacyDir)
	}
	if seen := st.MetaString("opencode_seen_time_updated_ms"); seen != "" {
		t.Fatalf("after unwire: seen = %q, want empty (cleared from legacy archive)", seen)
	}
}

// Unwire is the consent switch: after `witness unwire opencode`, no drain imports OpenCode again,
// even though an earlier import had recorded what it saw, and status no longer checks it.
func TestUnwireStopsOpenCodeCapture(t *testing.T) {
	drainEnv(t)
	t.Setenv("OPENCODE_CONFIG", "")
	dbPath := seedOpenCodeV2DB(t, 5000)
	t.Setenv("WITNESS_OPENCODE_DB", dbPath)
	plantOpenCodePlugin(t, "// wired")

	drainLog(t)
	if got := rawCount(t, "opencode:ses_fixture"); got != 2 {
		t.Fatalf("precondition: wired drain imported %d records, want 2", got)
	}

	var unwireErr error
	out := captureStdout(t, func() { unwireErr = cmdUninstall([]string{"opencode"}) })
	if unwireErr != nil {
		t.Fatal(unwireErr)
	}
	if !strings.Contains(out, "stopped") || !strings.Contains(out, "drains no longer import OpenCode") {
		t.Errorf("unwire output does not say capture stopped:\n%s", out)
	}

	addOpenCodeSession(t, dbPath, "ses_after", 9000)
	drainLog(t)
	if got := rawCount(t, "opencode:ses_after"); got != 0 {
		t.Fatalf("a drain after unwire imported %d records of a new OpenCode session, want 0", got)
	}
	if checks := openCodeChecks(t, true); checks != nil {
		t.Errorf("status still checks an unwired OpenCode: %+v", checks)
	}
}
