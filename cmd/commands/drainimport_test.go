package commands

import (
	"os"
	"path/filepath"
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

// TestDrainStartImportNeverFailsTheDrain: even when the drain-start import fails (v1 DB,
// missing DB, schema mismatch), the drain proceeds with what was already captured. Import
// errors are logged but never surfaced as drain errors.
func TestDrainStartImportNeverFailsTheDrain(t *testing.T) {
	t.Setenv("WITNESS_HOME", filepath.Join(t.TempDir(), "witness"))
	t.Setenv("WITNESS_NO_DEFAULT_LENS", "1")
	configDir := t.TempDir()
	t.Setenv("OPENCODE_CONFIG_DIR", configDir)

	// Point at a v1-only DB (no session_v2 table), which ProbeSchema will reject.
	v1Path := filepath.Join(t.TempDir(), "v1.db")
	if err := os.WriteFile(v1Path, []byte("SQLite format 3\x00"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("WITNESS_OPENCODE_DB", v1Path)

	// Wire OpenCode so ImportAtDrainStart is true.
	pluginPath := filepath.Join(configDir, "plugins", opencode.PluginFileName)
	if err := os.MkdirAll(filepath.Dir(pluginPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pluginPath, []byte("// wired"), 0o644); err != nil {
		t.Fatal(err)
	}

	// The drain must succeed even though the import will fail.
	ran, err := runWorker(false)
	if err != nil {
		t.Fatalf("drain must succeed despite import failure, got err=%v", err)
	}
	if !ran {
		t.Fatal("worker should have run")
	}
}
