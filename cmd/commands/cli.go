// Package commands implements the witness CLI: one cobra command per file,
// shared helpers here. The package is consumed by cmd/witness/main.go via Run().
package commands

import (
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/IngTian/witness/internal/proc"
	"github.com/IngTian/witness/internal/store"
)

// procCtl is the process-control port (issue #43): spawning the detached worker,
// terminating it, and the worker's signal-aware stop context all route through
// proc.Control instead of the old detach_*/procsignal_* //go:build files that
// reached into syscall directly. A package var so tests can swap in a proc.Fake to
// drive these paths without spawning real processes.
var procCtl proc.Control = proc.System()

// emitJSON marshals v as indented JSON to stdout. Used by read commands in --json
// mode; failures (always a marshaling issue, never a domain error) bubble up so
// Run's reportError still controls the exit code.
func emitJSON(v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	fmt.Println(string(b))
	return nil
}

// valueOrNever renders an empty timestamp as "never" for human-facing output.
func valueOrNever(v string) string {
	if strings.TrimSpace(v) == "" {
		return "never"
	}
	return v
}

// logLevel is the witness.log verbosity, INFO unless WITNESS_LOG_LEVEL says otherwise.
//
// It exists because the level was hardcoded to INFO and there was no way to raise it — so every
// slog.Debug in the tree was unreachable, including the five this branch added to the OpenCode
// reaper and the generation-phase transition. Those are exactly the lines someone wants when a
// distill misbehaves on a machine they cannot attach a debugger to, and "recompile witness" is not
// a diagnostic step a user can take.
//
// An env var rather than a config key on purpose: setupLogging runs before any command has parsed
// flags, and the hook path (capture/session-start/session-end) must stay allocation-cheap and
// argument-free. An unrecognized value falls back to INFO rather than failing — a typo in a
// diagnostic knob must never stop witness from capturing.
func logLevel() slog.Level {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("WITNESS_LOG_LEVEL"))) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

// setupLogging points slog at WITNESS_HOME/witness.log (JSON lines, append) and
// returns a closer. Each subcommand runs as its own process and configures its
// own default logger; failures that hooks would otherwise swallow land here.
func setupLogging(st *store.Store) func() {
	f, err := os.OpenFile(st.LogPath(), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return func() {}
	}
	slog.SetDefault(slog.New(slog.NewJSONHandler(f, &slog.HandlerOptions{Level: logLevel()})))
	return func() { _ = f.Close() }
}

// underTest reports whether this process is a `go test` binary rather than the real witness
// executable.
//
// It exists because spawnDetached re-execs os.Executable() — which under `go test` is the
// TEST binary, not witness. That test binary re-runs the whole package's TestMain with
// `worker-run` as its argv, does not recognize the flag, and (being setsid'd and Released)
// is fully orphaned: nothing reaps it, and `go test` cannot kill it either. Measured on a
// real machine: a single `go test ./...` left 746 orphaned `commands.test worker-run`
// processes and drove load average past 20; the run then blew its own 600s timeout, in a
// package that passes in ~1.3s when quiet.
//
// The hazard was already known and worked around per-test (see the comment in
// autoworker_stopflag_test.go) rather than prevented, so every NEW test that happened to
// reach a spawn point re-created it. Detecting it here fixes it once, for every caller.
//
// Detection uses the two signals the Go toolchain itself guarantees: the test binary's name
// always ends in ".test" (".test.exe" on Windows), and `go test` always defines -test.*
// flags in the default FlagSet. Either alone is enough; both together avoid depending on a
// single implementation detail. Deliberately NOT an env var a user could set, since that
// would let a misconfigured environment silently disable real distillation.
func underTest() bool {
	if flag.Lookup("test.v") != nil || flag.Lookup("test.run") != nil {
		return true
	}
	exe, err := os.Executable()
	if err != nil {
		return false
	}
	return looksLikeTestBinary(exe)
}

// looksLikeTestBinary is the NAME half of underTest, split out because it is the only half that
// ever runs in production: the -test.* flags are never registered in the real witness binary, so
// underTest's flag short-circuit always misses and this decides. Inlined, it was untestable —
// every test reaches underTest through the flag branch, so the production branch had no coverage
// and the "real binary" test could only re-implement the predicate and assert the copy against
// itself, i.e. it passed even with underTest rewritten to `return true` (which would silently
// disable distillation for every user). As a named function it can be table-tested directly.
func looksLikeTestBinary(exe string) bool {
	base := strings.ToLower(filepath.Base(exe))
	return strings.HasSuffix(base, ".test") || strings.HasSuffix(base, ".test.exe")
}

// spawnDetached re-execs this binary with the given args as a detached process,
// so hooks return instantly and the heavy work never blocks the session.
//
// Under `go test` this is a NO-OP: see underTest for why (it would fork the test binary into
// an unreapable orphan, 746 of them in one measured run). Tests that need to assert a spawn
// was requested should assert on the state the caller is responsible for — the queue row,
// the cleared stop flag — or inject a seam, not on a real child process.
func spawnDetached(args ...string) { _ = spawnDetachedOK(args...) }

// spawnDetachedOK is spawnDetached that REPORTS whether the child started.
//
// Callers that record "a child is now running" in durable state need this: the wakeup
// scheduler stamped its meta key before spawning and discarded the error, so a failed spawn
// left a phantom latch that suppressed the retry for the whole backoff window (see
// scheduleWorkerWakeupWith). Fire-and-forget callers keep using spawnDetached.
//
// Under `go test` it is a no-op that reports SUCCESS: the spawn is suppressed deliberately
// (see underTest), so reporting failure would make every test log a spurious "retry not
// armed" error and would change the state the caller writes — the opposite of inert.
func spawnDetachedOK(args ...string) bool {
	if underTest() {
		return true
	}
	exe, err := os.Executable()
	if err != nil {
		slog.Error("could not resolve our own executable to spawn a detached child", "err", err)
		return false
	}
	cmd := exec.Command(exe, args...)
	cmd.Env = os.Environ()
	cmd.Stdin = nil
	cmd.Stdout = nil
	cmd.Stderr = nil
	// Put the worker in its own session/process group so a SessionEnd-on-tab-close
	// doesn't kill it mid-distillation. proc.Detach is GOOS-split behind the port
	// (setsid on Unix; DETACHED_PROCESS|NEW_PROCESS_GROUP on Windows).
	procCtl.Detach(cmd)
	if err := cmd.Start(); err != nil {
		slog.Error("could not spawn detached child", "args", args, "err", err)
		return false
	}
	if cmd.Process != nil {
		_ = cmd.Process.Release()
	}
	return true
}

// reportError prints err in the format matching the caller's output mode: a JSON
// object {"error": "..."} on stderr when --json was passed, otherwise the plain
// "witness: ..." style. We sniff os.Args (not a threaded flag) because cobra has
// already finished parsing by the time RunE returns here, and the contract is
// simple — any --json anywhere means JSON for both success and failure.
func reportError(err error) {
	if jsonOutputMode() {
		b, _ := json.Marshal(map[string]string{"error": err.Error()})
		fmt.Fprintln(os.Stderr, string(b))
		return
	}
	fmt.Fprintln(os.Stderr, "witness:", err)
}

func jsonOutputMode() bool {
	for _, a := range os.Args[1:] {
		if a == "--json" || strings.HasPrefix(a, "--json=") {
			return true
		}
	}
	return false
}

// The single-flight consumer's drain loop lives in distill.Worker.Drain now — it
// owns the same contract (attempt each pending job once per run, pick up mid-run
// arrivals, terminate on a stuck job, optional budget cap) plus the MAP/REDUCE
// parallel split. The caller still holds the single-flight lock for the whole
// drain, so only one runs at a time across the machine; extra triggers no-op.

// agentFlag parses a minimal --agent <name> argument list for the internal
// capture command. Returns def when --agent is absent.
func agentFlag(args []string, def string) (string, error) {
	agent := def
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--agent":
			if i+1 >= len(args) || strings.TrimSpace(args[i+1]) == "" {
				return "", fmt.Errorf("--agent requires a value")
			}
			agent = strings.ToLower(strings.TrimSpace(args[i+1]))
			i++
		default:
			return "", fmt.Errorf("unknown argument %q", args[i])
		}
	}
	return agent, nil
}
