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

// worker-run's --auto must reach cmdWorker as auto. Every hook kick spawns `worker-run --auto`
// (maybeSpawnAutoWorker), and only an auto run honours the `worker stop --auto-only` latch; a
// manual run (the plain `worker-run` that import, ingest and distill spawn) clears it. So the
// latch tells the two apart: mapped to false, a hook-spawned worker would clear the latch and
// distil after the user stopped it; mapped to true, a manual kick would exit on the latch.
// The --since range keeps the run from arming a retry wakeup.
func TestWorkerRunAutoMapsToAuto(t *testing.T) {
	t.Setenv("WITNESS_HOME", filepath.Join(t.TempDir(), "witness"))
	t.Setenv("WITNESS_PROMPTS", filepath.Join("..", "..", "prompts"))
	run := func(args ...string) string {
		t.Helper()
		st, err := store.Open()
		if err != nil {
			t.Fatal(err)
		}
		defer st.Close()
		if err := st.SetMetaString("worker_stop_requested", "1"); err != nil {
			t.Fatal(err)
		}
		c := newInternalWorkerCmd()
		c.SetArgs(args)
		c.SilenceUsage, c.SilenceErrors = true, true
		if err := c.Execute(); err != nil {
			t.Fatalf("worker-run %v: %v", args, err)
		}
		return st.MetaString("worker_stop_requested")
	}
	if got := run("--auto", "--since", "3650d"); got != "1" {
		t.Fatalf("worker-run --auto cleared the stop latch (ran as manual): %q", got)
	}
	if got := run("--since", "3650d"); got != "" {
		t.Fatalf("worker-run without --auto kept the stop latch (ran as auto): %q", got)
	}
}

// import's hidden --auto routes the kick through the automatic gate, which clears the stop latch
// and here declines (no embedding model) instead of spawning the plain manual worker.
func TestImportAutoMapsToTheAutoGate(t *testing.T) {
	t.Setenv("WITNESS_HOME", filepath.Join(t.TempDir(), "witness"))
	t.Setenv("WITNESS_PROMPTS", filepath.Join("..", "..", "prompts"))
	st, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.AppendRaw(store.RawRecord{Session: "pending", Seq: 0, Role: "user", Text: "keep this in L0"}); err != nil {
		t.Fatal(err)
	}
	if err := st.SetMetaString("worker_stop_requested", "1"); err != nil {
		t.Fatal(err)
	}
	previous := importWorkerSpawner
	spawns := 0
	importWorkerSpawner = func() { spawns++ }
	t.Cleanup(func() { importWorkerSpawner = previous })

	c := newImportCmd()
	c.SetArgs([]string{"--agent", "claude", "--quiet", "--auto"})
	c.SilenceUsage, c.SilenceErrors = true, true
	if _, err := captureStdoutErr(t, c.Execute); err != nil {
		t.Fatal(err)
	}
	if got := st.MetaString("worker_stop_requested"); spawns != 0 || got != "" {
		t.Fatalf("import --auto: spawns=%d latch=%q, want the auto gate (no manual spawn, latch cleared)", spawns, got)
	}
}
