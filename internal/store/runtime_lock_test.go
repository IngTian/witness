package store

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"testing"
	"time"
)

func TestRuntimeLockSpansProcessesAndDiesWithHolder(t *testing.T) {
	root := t.TempDir()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, exe, "-test.run=^TestRuntimeLockChild$")
	cmd.Env = append(os.Environ(), "WITNESS_RUNTIME_LOCK_TEST="+root)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	scanner := bufio.NewScanner(stdout)
	if !scanner.Scan() || scanner.Text() != "locked" {
		t.Fatal("child never acquired lock")
	}
	waitCtx, stop := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer stop()
	if release, err := RuntimeLock(waitCtx, root); err == nil {
		release()
		t.Fatal("acquired a live child's lock")
	}
	_ = cmd.Process.Kill()
	_ = cmd.Wait()
	waitCtx, stop2 := context.WithTimeout(context.Background(), time.Second)
	defer stop2()
	release, err := RuntimeLock(waitCtx, root)
	if err != nil {
		t.Fatal("dead child stranded the login:", err)
	}
	release()
}

func TestRuntimeLockChild(t *testing.T) {
	root := os.Getenv("WITNESS_RUNTIME_LOCK_TEST")
	if root == "" {
		return
	}
	release, err := RuntimeLock(context.Background(), root)
	if err != nil {
		os.Exit(2)
	}
	defer release()
	fmt.Println("locked")
	time.Sleep(time.Minute)
}
