package commands

import (
	"errors"
	"testing"

	"github.com/IngTian/witness/internal/proc"
)

// TestTerminateWorkerDrivesPort proves the `distill stop` path drives the
// process-control port (issue #43) rather than calling syscall directly: with a
// proc.Fake swapped in, terminateWorker parses the pid and hands it to
// TerminateGroup, and no real signal is ever sent. It also covers the invalid-pid
// guard (rejected before the port is touched) and the error passthrough that
// cmdDistillStop turns into the "will exit at next checkpoint" fallback message.
func TestTerminateWorkerDrivesPort(t *testing.T) {
	prev := procCtl
	defer func() { procCtl = prev }()

	t.Run("valid pid reaches the port", func(t *testing.T) {
		fake := &proc.Fake{}
		procCtl = fake
		if err := terminateWorker("4242"); err != nil {
			t.Fatalf("terminateWorker: %v", err)
		}
		if len(fake.Terminated) != 1 || fake.Terminated[0] != 4242 {
			t.Fatalf("TerminateGroup not driven: %v", fake.Terminated)
		}
	})

	t.Run("invalid pid rejected before the port", func(t *testing.T) {
		fake := &proc.Fake{}
		procCtl = fake
		if err := terminateWorker("not-a-pid"); err == nil {
			t.Fatal("expected error for non-numeric pid")
		}
		if len(fake.Terminated) != 0 {
			t.Fatalf("port should not be touched for a bad pid: %v", fake.Terminated)
		}
	})

	t.Run("port error is surfaced", func(t *testing.T) {
		fake := &proc.Fake{TerminateErr: errors.New("no such process")}
		procCtl = fake
		if err := terminateWorker("999999"); err == nil {
			t.Fatal("expected the port's terminate error to propagate")
		}
	})
}
