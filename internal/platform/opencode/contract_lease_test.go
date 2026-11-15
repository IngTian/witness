//go:build opencodecontract

package opencode

import (
	"os"
	"testing"
	"time"
)

func TestContractLeaseKill(t *testing.T) { // T1: the holder dies uncleanly; serve must follow
	// On Linux, startHolder's serve also gets the parent-death signal (Pdeathsig), so this pass
	// proves "the serve follows its holder" partly through that signal. T2 is the lease-only proof.
	oc := requireContract(t)
	noLeaks(t)
	s := scratchEnv(t)
	holder, h := startHolder(t, oc.Bin, s, false, false)
	_ = holder.Process.Kill() // TerminateProcess / SIGKILL
	if !pidGone(h.ServePID, 2*time.Second) {
		t.Fatal("serve outlived its stdin holder by >2s")
	}
}

func TestContractLeaseClose(t *testing.T) { // T2: Close = close stdin, no WaitDelay needed
	oc := requireContract(t)
	noLeaks(t)
	s := scratchEnv(t)
	srv := startRawServe(t, oc.Bin, s)
	if srv.Cmd.WaitDelay != 0 {
		t.Fatal("T2 measures the lease alone; WaitDelay must be unset here")
	}
	start := time.Now()
	if err := srv.Close(); err != nil || time.Since(start) > 2*time.Second {
		t.Fatalf("close: %v after %s, want rc=0 within 2s", err, time.Since(start))
	}
}

func TestContractLeaseSiblingNoLeak(t *testing.T) { // T4: the write end must not leak into a sibling
	// On Linux, startHolder's serve also gets the parent-death signal (Pdeathsig), so this pass
	// proves "the serve follows its holder" partly through that signal. T2 is the lease-only proof.
	oc := requireContract(t)
	noLeaks(t)
	s := scratchEnv(t)
	holder, h := startHolder(t, oc.Bin, s, false, true)
	_ = holder.Process.Kill()
	if !pidGone(h.ServePID, 2*time.Second) {
		t.Fatal("serve survived while a sibling lived: the stdin write end leaked into it")
	}
	p, _ := os.FindProcess(h.SiblingPID)
	_ = p.Kill()
}
