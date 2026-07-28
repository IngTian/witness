package commands

import (
	"path/filepath"
	"testing"

	"github.com/IngTian/witness/internal/store"
)

func TestCmdImportNoKickLeavesPendingL0ForQuietWorker(t *testing.T) {
	t.Setenv("WITNESS_HOME", filepath.Join(t.TempDir(), "witness"))
	t.Setenv("WITNESS_PROMPTS", filepath.Join("..", "..", "prompts"))
	st, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	if err := st.AppendRaw(store.RawRecord{Session: "pending", Seq: 0, Role: "user", Text: "keep this in L0"}); err != nil {
		t.Fatal(err)
	}
	if got, _ := st.PendingSessions(activeLensNames(st)); len(got) != 1 {
		t.Fatalf("pending sessions before import = %v, want one", got)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	previousSpawner := importWorkerSpawner
	var spawns int
	importWorkerSpawner = func() { spawns++ }
	t.Cleanup(func() { importWorkerSpawner = previousSpawner })

	if err := cmdImport([]string{"--agent", "claude", "--quiet", "--no-kick"}); err != nil {
		t.Fatal(err)
	}
	if spawns != 0 {
		t.Fatalf("L0-only import spawned %d worker(s), want none", spawns)
	}
	st, err = store.Open()
	if err != nil {
		t.Fatal(err)
	}
	if got := st.RawCount("pending"); got != 1 {
		t.Fatalf("L0-only import changed pending raw count to %d, want 1", got)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	if err := cmdImport([]string{"--agent", "claude", "--quiet"}); err != nil {
		t.Fatal(err)
	}
	if spawns != 1 {
		t.Fatalf("ordinary import spawned %d worker(s), want one", spawns)
	}
}
