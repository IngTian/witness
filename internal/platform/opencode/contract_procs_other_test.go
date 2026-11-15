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

// procPIDs lists the live processes whose argv[0] basename is name (procName-normalized).
// argv[0], not the kernel's comm: comm truncates (15 bytes on Linux), and `opencode-pty` and a
// long witness binary name would fall to it.
func procPIDs(t *testing.T, name string) []int {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "ps", "-A", "-o", "pid=,args=").Output()
	if err != nil {
		t.Fatalf("ps: %v", err)
	}
	want := procName(name)
	var pids []int
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Fields(line)
		if len(f) < 2 || procName(filepath.Base(f[1])) != want {
			continue
		}
		if pid, err := strconv.Atoi(f[0]); err == nil {
			pids = append(pids, pid)
		}
	}
	return pids
}
