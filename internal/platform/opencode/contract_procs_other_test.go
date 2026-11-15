//go:build opencodecontract && !windows

package opencode

import (
	"context"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// procList maps every live pid to the procName of its argv[0] basename, from one `ps`. argv[0],
// not the kernel's comm: comm truncates (15 bytes on Linux), and `opencode-pty` and a long witness
// binary name would fall to it.
func procList(t *testing.T) map[int]string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "ps", "-A", "-o", "pid=,args=").Output()
	if err != nil {
		t.Fatalf("ps: %v", err)
	}
	procs := map[int]string{}
	for _, line := range strings.Split(string(out), "\n") {
		if f := strings.Fields(line); len(f) >= 2 {
			if pid, err := strconv.Atoi(f[0]); err == nil {
				procs[pid] = procName(filepath.Base(f[1]))
			}
		}
	}
	return procs
}

// procArgs is pid's command line, or "" if ps cannot read it (most often: the pid is gone). Never
// fatal: it runs in Cleanups, where it decides whether a kill is safe.
func procArgs(t *testing.T, pid int) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, _ := exec.CommandContext(ctx, "ps", "-p", strconv.Itoa(pid), "-o", "args=").Output()
	return strings.TrimSpace(string(out))
}
