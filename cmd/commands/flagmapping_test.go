package commands

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/IngTian/witness/internal/store"
)

// The OpenCode plugin's background import is `import --agent opencode --quiet --no-kick
// --session …`, and the tests call cmdImport(importOpts{…}) directly. So this drives the cobra
// command itself, to pin each flag to the field it sets: with --quiet and --no-kick tested apart,
// swapping them in RunE flips both outcomes.
func TestImportFlagsMapToTheirOptions(t *testing.T) {
	t.Setenv("WITNESS_HOME", filepath.Join(t.TempDir(), "witness"))
	t.Setenv("WITNESS_PROMPTS", filepath.Join("..", "..", "prompts"))
	st, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	// Pending L0, so an import that may kick does kick.
	if err := st.AppendRaw(store.RawRecord{Session: "pending", Seq: 0, Role: "user", Text: "keep this in L0"}); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	previous := importWorkerSpawner
	spawns := 0
	importWorkerSpawner = func() { spawns++ }
	t.Cleanup(func() { importWorkerSpawner = previous })

	run := func(args ...string) (string, error) {
		c := newImportCmd()
		c.SetArgs(args)
		c.SilenceUsage, c.SilenceErrors = true, true
		return captureStdoutErr(t, c.Execute)
	}

	out, err := run("--agent", "claude", "--no-kick")
	if err != nil || out == "" || spawns != 0 {
		t.Fatalf("--no-kick: err=%v stdout=%q spawns=%d, want a status line and no worker", err, out, spawns)
	}
	out, err = run("--agent", "claude", "--quiet")
	if err != nil || out != "" || spawns != 1 {
		t.Fatalf("--quiet: err=%v stdout=%q spawns=%d, want silence and one worker", err, out, spawns)
	}
	if _, err := run("--agent", "claude", "--session", " "); err == nil || !strings.Contains(err.Error(), "--session requires a value") {
		t.Fatalf("a blank --session must be refused (proves --session reaches cmdImport), got %v", err)
	}
}

// worker-run's --since and --until must land on their own bounds. A bad --since fails at the
// parse, before any store or runner work, so this needs no worker.
func TestWorkerRunSinceUntilMapToTheirBounds(t *testing.T) {
	for flag, want := range map[string]string{"--since": "invalid --since", "--until": "invalid --until"} {
		c := newInternalWorkerCmd()
		c.SetArgs([]string{flag, "bogus"})
		c.SilenceUsage, c.SilenceErrors = true, true
		if err := c.Execute(); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("worker-run %s bogus: got %v, want %q", flag, err, want)
		}
	}
}
