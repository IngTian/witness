//go:build !windows

package commands

import (
	"fmt"
	"syscall"
)

// terminateWorkerPID stops the detached worker. It signals the worker's process
// GROUP first (SIGTERM to -n) — the worker is started with Setsid, so it leads
// its own group — then falls back to the bare pid. This mirrors the Unix detach
// in detach_unix.go: killing the group also reaps any `claude -p` child the
// worker spawned.
func terminateWorkerPID(n int) error {
	if err := syscall.Kill(-n, syscall.SIGTERM); err == nil {
		return nil
	}
	if err := syscall.Kill(n, syscall.SIGTERM); err != nil {
		return fmt.Errorf("terminate worker pid=%d: %w", n, err)
	}
	return nil
}
