// Package commands implements the witness CLI: one cobra command per file,
// shared helpers here. The package is consumed by cmd/witness/main.go via Run().
package commands

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
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

// setupLogging points slog at WITNESS_HOME/witness.log (JSON lines, append) and
// returns a closer. Each subcommand runs as its own process and configures its
// own default logger; failures that hooks would otherwise swallow land here.
func setupLogging(st *store.Store) func() {
	f, err := os.OpenFile(st.LogPath(), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return func() {}
	}
	slog.SetDefault(slog.New(slog.NewJSONHandler(f, &slog.HandlerOptions{Level: slog.LevelInfo})))
	return func() { _ = f.Close() }
}

// spawnDetached re-execs this binary with the given args as a detached process,
// so hooks return instantly and the heavy work never blocks the session.
func spawnDetached(args ...string) {
	exe, err := os.Executable()
	if err != nil {
		return
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
	_ = cmd.Start() // fire and forget
	if cmd.Process != nil {
		_ = cmd.Process.Release()
	}
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
