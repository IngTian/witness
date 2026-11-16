//go:build opencodecontract

package opencode

// T7: what the real witness binary's detached worker leaves behind, started the production way, by
// `ingest` (the path #216 was about):
//   - TestContractWorkerStopTakesChildren: `worker stop` takes the worker's `claude -p` and its
//     private OpenCode serve along (the pty daemon is T8's). Linux: the stop's group SIGTERM. Windows:
//     the kill-on-close job the worker puts itself in (Tasks 3.1/3.2), since TerminateProcess
//     neither cascades nor runs the worker's defers.
//   - TestContractWakeupOutlivesWorker: the retry worker-wakeup a failed mine arms outlives the
//     worker that spawned it. Windows: Detach's CREATE_BREAKAWAY_FROM_JOB, or the job's close at
//     the worker's exit takes it.
//
// Processes are counted against a snapshot taken at the test's start, not by name alone: on a
// developer's machine their own opencode is running, and so is a witness built in another checkout
// under the same bin/witness-<os>-<arch> name.

import (
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/IngTian/witness/internal/embed"
	"github.com/IngTian/witness/internal/proc"
	"github.com/IngTian/witness/internal/store"
)

func TestContractWorkerStopTakesChildren(t *testing.T) {
	requireContract(t)
	requireEmbedModel(t)
	noLeaks(t)
	before := procList(t)
	s := scratchEnv(t)
	s.setProcessEnv(t)
	m := startMock(t)
	installSystemProvider(t, m) // skips on darwin
	// sleep: a `claude -p` that never answers, so the stop finds it running.
	env := startT7Worker(t, s, "sleep", `{"dimensions":["speed"],"runner":"opencode","extract_model":"mock/ok","review_model":"mock/ok"}`)
	var claude []int
	waitFor(t, 3*time.Minute, "the claude stub and the worker's private serve running (no serve at all: the machine-wide mock "+
		"provider config may not have loaded into the worker's private serve, so its Open refused mock/ok; witness.log is logged below)", func() bool {
		claudeProcs := fresh(t, before, "claude")
		claude = slices.Collect(maps.Keys(claudeProcs))
		slices.Sort(claude)
		return len(claude) == 1 && len(fresh(t, before, "opencode")) >= 1
	})
	// The stub must still be running when the stop lands: one that died early (a quarantined copy, a
	// mistyped stub mode) would read as taken.
	if !proc.Alive(claude[0]) {
		t.Fatalf("the claude stub %d died before `worker stop`", claude[0])
	}
	runWitness(t, s, env, "worker", "stop")
	if !pidGone(claude[0], 10*time.Second) {
		t.Fatalf("the claude stub %d outlived `worker stop` by 10 s", claude[0])
	}
	waitFor(t, 10*time.Second, "no claude stub, serve or witness left after `worker stop`", func() bool {
		return len(fresh(t, before, "claude", "opencode", "opencode-pty", witnessName())) == 0
	})
}

func TestContractWakeupOutlivesWorker(t *testing.T) {
	requireContract(t)
	requireEmbedModel(t)
	noLeaks(t)
	before := procList(t)
	s := scratchEnv(t)
	s.setProcessEnv(t) // store.Open below opens the scratch archive
	// The default lens alone, on the claude runner: its `claude -p` fails, a real failure (no stop
	// was asked for), so the session backs off and the worker arms the retry wakeup on exit.
	startT7Worker(t, s, "fail", "")
	worker := 0 // its stamped pid, read while it runs (its graceful exit clears it)
	waitFor(t, 3*time.Minute, "the worker exited with a retry wakeup armed", func() bool {
		st, err := store.Open()
		if err != nil {
			return false
		}
		defer st.Close()
		if pid, err := strconv.Atoi(st.MetaString("worker_pid")); err == nil {
			worker = pid
		}
		// The stamp first: WorkerActive briefly holds the worker lock, and probing it before the
		// worker has taken it could make that worker lose the lock and no-op.
		return st.MetaString("worker_manual_wakeup_at") != "" && !st.WorkerActive()
	})
	// Past the worker's exit, which on Windows closes its job: a worker still tearing down (the
	// embedder unloading) would count as a second process below.
	if worker == 0 || !pidGone(worker, 30*time.Second) {
		t.Fatalf("worker pid %d: never seen stamped, or still alive 30 s after it released its lock", worker)
	}
	// Windows snapshots can list exited-but-referenced processes for a moment; wait until the worker
	// is absent from the snapshot, not just !Alive, before counting new witness processes.
	for deadline := time.Now().Add(30 * time.Second); procList(t)[worker] != ""; {
		if time.Now().After(deadline) {
			t.Fatalf("worker %d still in process snapshot 30 s after it exited", worker)
		}
		time.Sleep(100 * time.Millisecond)
	}
	witnessProcs := fresh(t, before, procName(witnessName()))
	pids := slices.Collect(maps.Keys(witnessProcs))
	slices.Sort(pids)
	if len(pids) != 1 {
		t.Fatalf("%d new witness processes %v after the worker %d exited, want exactly the sleeping worker-wakeup", len(pids), pids, worker)
	}
	p, err := os.FindProcess(pids[0])
	if err != nil {
		t.Fatalf("worker-wakeup %d: %v", pids[0], err)
	}
	t.Cleanup(func() {
		// Outside CI the snapshot diff says only "new": a same-named witness the owner started during
		// the test is new too. So kill only a pid whose command line says worker-wakeup; on Windows
		// (no command line from Toolhelp32) that is never, and the wakeup is left to sleep out.
		if args := procArgs(t, pids[0]); os.Getenv("CI") != "true" && !strings.Contains(args, "worker-wakeup") {
			t.Logf("not killing pid %d outside CI: its command line %q does not say worker-wakeup", pids[0], args)
			return
		}
		_ = p.Kill()
	})
}

// requireEmbedModel: the worker loads the embedding model before its first runner call, so without
// it neither test reaches a `claude -p`. The contract workflow fetches it into WITNESS_ASSETS; a CI
// run without it must go red, a developer's run skips.
func requireEmbedModel(t *testing.T) {
	t.Helper()
	if embed.ModelReady() {
		return
	}
	msg := fmt.Sprintf("no embedding model at %s: point WITNESS_ASSETS at a `witness model fetch` download", embed.AssetsDir())
	if os.Getenv("CI") == "true" {
		t.Fatal(msg)
	}
	t.Skip(msg)
}

// startT7Worker readies the scratch archive and ingests one session, whose ingest spawns the
// detached worker-run. stub is what the `claude` stand-in first on PATH does; lensJSON, if set,
// is registered and enabled as lens t7oc beside the default one, with the example lens's prompts.
// It returns the env every later witness call needs. At cleanup it stops the worker, so a failure
// mid-test leaves none running, and on a failure it logs the tail of witness.log, which goes with
// the scratch dir.
func startT7Worker(t *testing.T, s scratch, stub, lensJSON string) []string {
	t.Helper()
	env := []string{"PATH=" + filepath.Dir(stubPath(t, "claude")) + string(os.PathListSeparator) + os.Getenv("PATH"),
		"WITNESS_CONTRACT_STUB=" + stub}
	disableAutoDistill(t, s) // before any witness runs
	runWitness(t, s, env, "config", "set", "mine_model", "mock/ok")
	runWitness(t, s, env, "config", "set", "review_model", "mock/ok")
	if lensJSON != "" {
		dir := filepath.Join(s.Root, "t7oc")
		files := map[string][]byte{"lens.json": []byte(lensJSON)}
		for _, f := range []string{"extract.md", "review.md"} {
			b, err := os.ReadFile(filepath.Join("..", "..", "..", "prompts", "lens", "example", f))
			if err != nil {
				t.Fatal(err)
			}
			files[f] = b
		}
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		for f, b := range files {
			if err := os.WriteFile(filepath.Join(dir, f), b, 0o600); err != nil {
				t.Fatal(err)
			}
		}
		runWitness(t, s, env, "lens", "register", "t7oc", dir)
		runWitness(t, s, env, "lens", "enable", "t7oc")
	}
	ndjson := filepath.Join(s.Root, "t7.ndjson")
	if err := os.WriteFile(ndjson, []byte(`{"session":"t7","role":"user","text":"Refactor the parser, tests first."}`+"\n"+
		`{"session":"t7","role":"assistant","text":"Wrote three failing tests, then split the parser in two."}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { // runs after the stop below, so the log holds the stop too
		if t.Failed() {
			b, _ := os.ReadFile(filepath.Join(s.WitnessHome, "witness.log"))
			t.Logf("witness.log, last 4 KB:\n%s", b[max(0, len(b)-4096):])
		}
	})
	t.Cleanup(func() { runWitness(t, s, env, "worker", "stop") })
	runWitness(t, s, env, "ingest", "--quiet", "--file", ndjson)
	return env
}

// witnessName is the process name of the witness binary under test.
func witnessName() string { return filepath.Base(os.Getenv("WITNESS_CONTRACT_WITNESS_BIN")) }

// spawned lists the live processes named any of names that were not in before (a pid reused under
// another name counts as new, as in noLeaks), from one procList.
