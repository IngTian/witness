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

	"github.com/IngTian/witness/internal/store"
)

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
	// doesn't kill it mid-distillation. detachSysProcAttr is GOOS-split (setsid on
	// Unix; DETACHED_PROCESS|NEW_PROCESS_GROUP on Windows) — see detach_unix.go /
	// detach_windows.go.
	cmd.SysProcAttr = detachSysProcAttr()
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

// drainQueue is the single-flight consumer's core loop. It processes every job
// the `pending` source reports, exactly once per run, re-scanning after each pass
// so jobs that ARRIVE mid-run are still picked up. It terminates when a scan turns
// up nothing not-yet-attempted — so a job that stays pending (e.g. one being
// dead-lettered) is attempted once here, not spun on forever.
//
// The caller holds the single-flight lock for the duration, so only one of these
// runs at a time across the whole machine; extra triggers no-op instead of piling
// up as blocked processes.
func drainQueue(pending func() []string, process func(string)) {
	_ = drainQueueLimit(pending, process, 0)
}

// drainQueueLimit is drainQueue with an optional process budget. max <= 0 means
// unbounded. It is used for runners that cannot safely create many nested agent
// sessions in one background pass.
func drainQueueLimit(pending func() []string, process func(string), max int) int {
	attempted := map[string]bool{}
	processed := 0
	for {
		var next []string
		for _, job := range pending() {
			if !attempted[job] {
				next = append(next, job)
			}
		}
		if len(next) == 0 {
			return processed
		}
		for _, job := range next {
			if max > 0 && processed >= max {
				return processed
			}
			attempted[job] = true
			process(job)
			processed++
		}
	}
}

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
