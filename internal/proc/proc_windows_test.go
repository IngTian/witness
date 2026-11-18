//go:build windows

package proc

import (
	"errors"
	"os/exec"
	"slices"
	"syscall"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// HideWindow must set one field on the attr already there: the first #143 fix assigned a fresh
// SysProcAttr and dropped the flags another layer had set. A cmd with no attr gets one.
func TestHideWindowKeepsExistingFlags(t *testing.T) {
	cmd := exec.Command("cmd")
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_NEW_PROCESS_GROUP}
	HideWindow(cmd)
	if !cmd.SysProcAttr.HideWindow || cmd.SysProcAttr.CreationFlags != windows.CREATE_NEW_PROCESS_GROUP {
		t.Fatalf("HideWindow left %+v; want HideWindow plus the existing CREATE_NEW_PROCESS_GROUP", cmd.SysProcAttr)
	}
	bare := exec.Command("cmd")
	HideWindow(bare)
	if bare.SysProcAttr == nil || !bare.SysProcAttr.HideWindow {
		t.Fatal("HideWindow on a cmd with no SysProcAttr must allocate one and set HideWindow")
	}
}

// The flags must be EXACTLY kill-on-close + breakaway-ok. SILENT_BREAKAWAY_OK would let every
// child (claude -p, the serve) leave the job without asking, so closing it would kill nothing.
func TestWorkerJobLimits(t *testing.T) {
	job, err := newWorkerJob()
	if err != nil {
		t.Fatal(err)
	}
	defer windows.CloseHandle(job)
	var info windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION
	if err := windows.QueryInformationJobObject(job, windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info)), nil); err != nil {
		t.Fatal(err)
	}
	want := uint32(windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE | windows.JOB_OBJECT_LIMIT_BREAKAWAY_OK)
	if got := info.BasicLimitInformation.LimitFlags; got != want {
		t.Fatalf("LimitFlags = %#x, want exactly %#x (SILENT_BREAKAWAY_OK would let claude -p escape)", got, want)
	}
}

// Closing the last handle IS the kill: that is what the OS does for us when the worker dies.
func TestWorkerJobKillsMembersOnClose(t *testing.T) {
	job, err := newWorkerJob()
	if err != nil {
		t.Fatal(err)
	}
	cmd, exited := startPing(t, false)
	h, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(cmd.Process.Pid))
	if err == nil {
		err = windows.AssignProcessToJobObject(job, h)
		windows.CloseHandle(h)
	}
	if err == nil { // else a failed open/assign is the error, and may itself be why ping is gone
		select { // a ping that already died on its own would make the check below vacuous
		case <-exited:
			err = errors.New("ping exited before the job closed; the test would prove nothing")
		default:
		}
	}
	windows.CloseHandle(job) // the last handle: KILL_ON_JOB_CLOSE fires
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-exited:
	case <-time.After(2 * time.Second):
		t.Fatal("closing the job did not kill its member")
	}
}

// Breakaway only once contained: an uncontained spawner may sit in someone else's job that
// forbids breakaway, where the flag makes CreateProcess fail. Saves and restores `contained`, so
// it holds in any order relative to TestContainChildrenBreakawayEscapesJob.
func TestDetachBreaksAwayOnlyWhenContained(t *testing.T) {
	base := uint32(windows.CREATE_NEW_PROCESS_GROUP | windows.DETACHED_PROCESS)
	prev := contained.Load()
	t.Cleanup(func() { contained.Store(prev) })
	for _, c := range []struct {
		in   bool
		want uint32
	}{{false, base}, {true, base | windows.CREATE_BREAKAWAY_FROM_JOB}} {
		contained.Store(c.in)
		cmd := exec.Command("cmd")
		System().Detach(cmd)
		if cmd.SysProcAttr.CreationFlags != c.want {
			t.Errorf("contained=%v: flags %#x, want %#x", c.in, cmd.SysProcAttr.CreationFlags, c.want)
		}
	}
}

// Irreversible for this test binary (it stays in the job until it exits) — which only means its
// own children die with it.
func TestContainChildrenBreakawayEscapesJob(t *testing.T) {
	var first windows.Handle
	for i := 0; i < 2; i++ { // twice: a backfill re-runs the worker in-process
		if err := System().ContainChildren(); err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
		if i == 0 {
			first = workerJob
		} else if workerJob != first {
			t.Fatalf("the second call made a new job (%#x, was %#x): the Once does not hold", workerJob, first)
		}
	}
	inside, _ := startPing(t, false)
	// Nested jobs judge breakaway by the IMMEDIATE job (ours, BREAKAWAY_OK), so by Microsoft's docs
	// the Detach'd start also succeeds inside an outer job that forbids breakaway. That case is
	// untested: no run builds such an outer job.
	away, awayExited := startPing(t, true)
	pids := jobPIDs(t, workerJob)
	// Read after the listing, so it covers the listing itself: a child already gone is in no job,
	// and would read as broken away.
	select {
	case <-awayExited:
		t.Fatalf("the Detach'd child (pid %d) exited before the job was read: its absence proves nothing", away.Process.Pid)
	default:
	}
	if !slices.Contains(pids, uintptr(inside.Process.Pid)) {
		t.Errorf("a plain child (pid %d) is not in the worker's job %v", inside.Process.Pid, pids)
	}
	if slices.Contains(pids, uintptr(away.Process.Pid)) {
		t.Errorf("a Detach'd child (pid %d) stayed in the job: the wakeup would die with the worker", away.Process.Pid)
	}
}

// startPing starts a system binary bounded at ~30 s (Detach'd if asked) and kills it on every
// test exit path. One goroutine owns Wait; exited closes when the process is gone.
func startPing(t *testing.T, detach bool) (*exec.Cmd, <-chan struct{}) {
	t.Helper()
	cmd := exec.Command("ping", "-n", "30", "127.0.0.1")
	if detach {
		System().Detach(cmd)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start ping (detach=%v): %v", detach, err)
	}
	exited := make(chan struct{})
	go func() { _ = cmd.Wait(); close(exited) }()
	t.Cleanup(func() { _ = cmd.Process.Kill(); <-exited })
	return cmd, exited
}

// jobPIDs lists the job's processes (and its child jobs'). x/sys has no type for it, so this is
// JOBOBJECT_BASIC_PROCESS_ID_LIST as documented (DWORD, DWORD, ULONG_PTR[]):
// https://learn.microsoft.com/en-us/windows/win32/api/winnt/ns-winnt-jobobject_basic_process_id_list
func jobPIDs(t *testing.T, job windows.Handle) []uintptr {
	t.Helper()
	var list struct {
		Assigned, InList uint32
		PIDs             [64]uintptr
	}
	if err := windows.QueryInformationJobObject(job, windows.JobObjectBasicProcessIdList,
		uintptr(unsafe.Pointer(&list)), uint32(unsafe.Sizeof(list)), nil); err != nil {
		t.Fatal(err)
	}
	return list.PIDs[:list.InList]
}
