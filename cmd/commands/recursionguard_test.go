package commands

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/IngTian/witness/internal/store"
)

// The WITNESS_WORKER=1 recursion guard is a FROZEN CONTRACT (AGENTS.md) and had NO test.
//
// The worker runs `claude -p`, which re-fires the Claude Code hooks, which invoke `witness
// capture`. Without the guard those hooks CAPTURE THE WORKER'S OWN TRAFFIC: every distillation
// prompt and model reply lands in L0 under the nested session id and is then mined as if it
// were the user's work. That is the exact contamination class v0.7.0 eliminated for OpenCode
// via native session isolation — and it compounds, because polluted L0 becomes new pending work
// whose mining emits more polluted L0, so the archive and the model bill grow without bound
// while the profile drifts toward describing witness's own prompts.
//
// On Unix the shell shim checks too, so this is belt-and-suspenders. On WINDOWS there is no
// shim (exec-form hooks call the exe directly), so Run() is the ONLY line of defense.
//
// Deliberately spawn-free: `capture` has no spawnDetached call site (the full list is
// autoworker/distill/importcmd/ingest/observations), and underTest() would make one inert
// anyway — so this cannot leak processes, which matters in a repo that has already had two
// runaway-process incidents.
func TestRecursionGuardStopsCaptureInsideAWorkerSubprocess(t *testing.T) {
	home := t.TempDir()
	t.Setenv("WITNESS_HOME", home)
	t.Setenv("WITNESS_PROMPTS", filepath.Join("..", "..", "prompts"))
	t.Setenv("WITNESS_WORKER", "1")

	payload := `{"session_id":"nested-worker-session","prompt":"a distillation prompt witness itself sent"}`
	withArgsAndStdin(t, []string{"witness", "capture"}, payload)

	if code := Run(); code != 0 {
		t.Errorf("the guard must exit 0 (capture is best-effort and must never break a session), got %d", code)
	}

	// The load-bearing assertion: NOTHING was written.
	if n := rawRowCount(t); n != 0 {
		t.Errorf("the worker's own turn was captured into L0 (%d raw rows) — this is the "+
			"self-feed loop that pollutes the archive and compounds every run", n)
	}
}

// The negative control: without the env var, the SAME payload IS captured. Without this, a guard
// that rejected everything would pass the test above while breaking all capture.
func TestCaptureStillWorksWithoutTheWorkerEnv(t *testing.T) {
	home := t.TempDir()
	t.Setenv("WITNESS_HOME", home)
	t.Setenv("WITNESS_PROMPTS", filepath.Join("..", "..", "prompts"))
	os.Unsetenv("WITNESS_WORKER")

	payload := `{"session_id":"a-real-user-session","prompt":"the user actually typed this"}`
	withArgsAndStdin(t, []string{"witness", "capture"}, payload)

	if code := Run(); code != 0 {
		t.Fatalf("capture should succeed, got exit %d", code)
	}
	if n := rawRowCount(t); n == 0 {
		t.Error("a genuine user turn was NOT captured — the guard is over-broad and capture is dead")
	}
}

// `doctor` is deliberately EXEMPT so a user can diagnose from inside a worker-spawned shell.
// The exemption is one `os.Args[1] != "doctor"` clause and is easy to drop by accident.
func TestRecursionGuardExemptsDoctor(t *testing.T) {
	t.Setenv("WITNESS_HOME", t.TempDir())
	t.Setenv("WITNESS_PROMPTS", filepath.Join("..", "..", "prompts"))
	t.Setenv("WITNESS_ASSETS", t.TempDir()) // no model => doctor reports unhealthy and exits 1
	t.Setenv("WITNESS_WORKER", "1")

	withArgsAndStdin(t, []string{"witness", "doctor"}, "")
	// The guard would return 0 without even running doctor. Reaching doctor's own unhealthy
	// exit code (1) proves the exemption held — a 0 here would mean the guard swallowed it.
	if code := Run(); code != 1 {
		t.Errorf("doctor must run despite WITNESS_WORKER=1 (it is the diagnostic escape hatch); "+
			"got exit %d, which means the guard intercepted it", code)
	}
}

// Every OTHER command is guarded, not just capture — the guard keys on the env var, so a new
// subcommand is covered automatically and must stay that way.
func TestRecursionGuardCoversOtherCommands(t *testing.T) {
	for _, tok := range []string{"capture", "session-start", "session-end", "worker-run"} {
		t.Run(tok, func(t *testing.T) {
			t.Setenv("WITNESS_HOME", t.TempDir())
			t.Setenv("WITNESS_PROMPTS", filepath.Join("..", "..", "prompts"))
			t.Setenv("WITNESS_WORKER", "1")
			withArgsAndStdin(t, []string{"witness", tok}, `{"session_id":"s","prompt":"x"}`)
			if code := Run(); code != 0 {
				t.Errorf("%s must be short-circuited to 0 inside a worker subprocess, got %d", tok, code)
			}
			if n := rawRowCount(t); n != 0 {
				t.Errorf("%s wrote %d raw rows inside a worker subprocess", tok, n)
			}
		})
	}
}

// withArgsAndStdin points os.Args and os.Stdin at the given values for one Run(), restoring both.
// Stdin is a real temp FILE (not a pipe) so the guard's io.Copy-to-discard cannot block.
func withArgsAndStdin(t *testing.T, args []string, stdin string) {
	t.Helper()
	prevArgs, prevStdin := os.Args, os.Stdin
	t.Cleanup(func() { os.Args, os.Stdin = prevArgs, prevStdin })

	f, err := os.CreateTemp(t.TempDir(), "stdin")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(stdin); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Seek(0, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.Close() })
	os.Args, os.Stdin = args, f
}

func rawRowCount(t *testing.T) int {
	t.Helper()
	st, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	stats := st.Stats(activeLensNames(st))
	return stats.RawRecords
}
