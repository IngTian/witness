package commands

import (
	"path/filepath"
	"testing"

	"github.com/IngTian/witness/internal/store"
)

// `worker stop --auto-only` (what the OpenCode plugin runs on dispose) latches a
// DURABLE worker_stop_requested meta flag, and cmdWorker refuses to run an AUTO worker
// while it is set. Only maybeSpawnAutoWorker (the shared kick gate) and a MANUAL run
// clear it — so any caller that spawns `worker-run --auto` itself no-ops forever after
// the first dispose, silently freezing automatic distillation for an OpenCode-only user
// (a Claude Code user is rescued incidentally by their capture hook clearing it).
//
// This locks the clear: the kick gate must reset the flag so a post-dispose start works.
func TestAutoWorkerKickClearsStopFlagAfterDispose(t *testing.T) {
	t.Setenv("WITNESS_HOME", filepath.Join(t.TempDir(), "witness"))
	t.Setenv("WITNESS_PROMPTS", filepath.Join("..", "..", "prompts"))
	t.Setenv("WITNESS_ASSETS", t.TempDir())
	st, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := seedDefaultLens(st); err != nil {
		t.Fatalf("seedDefaultLens: %v", err)
	}
	// Pending work so the gate has a reason to start a worker.
	if err := st.AppendRaw(store.RawRecord{Session: "s", Seq: 0, Role: "user", Text: "hi"}); err != nil {
		t.Fatal(err)
	}

	// Dispose (OpenCode close) latches the durable stop flag.
	if err := cmdDistillStop(true); err != nil {
		t.Fatalf("stop --auto-only: %v", err)
	}
	if got := st.MetaString("worker_stop_requested"); got != "1" {
		t.Fatalf("precondition: dispose should latch the stop flag, got %q", got)
	}

	// The plugin's quiet-period start routes through the kick gate, which must CLEAR the
	// latched flag. (spawnDetached re-execs the test binary in-process terms, so we assert
	// on the flag the gate is responsible for, not on a spawned child.)
	maybeSpawnAutoWorker(st)

	if got := st.MetaString("worker_stop_requested"); got != "" {
		t.Fatalf("kick gate must clear the stop flag (else auto-distill is frozen forever), got %q", got)
	}
}
