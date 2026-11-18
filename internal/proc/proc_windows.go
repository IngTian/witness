//go:build windows

package proc

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sync"
	"sync/atomic"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// The worker's job, set once by ContainChildren and NEVER closed: closing the last handle IS
// the kill, so the OS does it when this process dies — by return, crash or TerminateProcess.
var (
	containOnce sync.Once
	containErr  error
	workerJob   windows.Handle
	contained   atomic.Bool // read by Detach on any goroutine
)

// Detach is the Windows analogue of Unix setsid for the detached worker.
// DETACHED_PROCESS gives the child no console, so a console-close CTRL_CLOSE_EVENT
// can't reach it mid-distillation; CREATE_NEW_PROCESS_GROUP isolates it from the
// Ctrl-C/Ctrl-Break sent to the parent's group. Together they detach the worker
// from the launching terminal the way Setsid does on Unix. (The SessionStart
// backlog sweep is the fallback recovery path on either platform.)
//
// Once this process is contained, CREATE_BREAKAWAY_FROM_JOB too: a Detach'd child (a worker-run,
// the retry wakeup) exists to outlive its spawner, so it must leave the job that kills the
// worker. Only then: an uncontained spawner may sit in someone else's job that forbids
// breakaway, where the flag makes CreateProcess fail (libuv skips it for the same reason).
//
// ponytail: inside an outer job that forbids breakaway, the wakeup still starts — nested jobs
// judge the flag by the IMMEDIATE job (ours, BREAKAWAY_OK), and the child climbs out only until
// it meets a job that forbids it — but it then lives in that outer job and dies when that job
// closes. Upgrade: skip containment when IsProcessInJob finds such an outer job.
func (sys) Detach(cmd *exec.Cmd) {
	flags := uint32(windows.CREATE_NEW_PROCESS_GROUP | windows.DETACHED_PROCESS)
	if contained.Load() {
		flags |= windows.CREATE_BREAKAWAY_FROM_JOB
	}
	ensureSysProcAttr(cmd).CreationFlags |= flags
}

// HideWindow keeps a console child (claude.exe, codex.exe, opencode.exe) from showing a window.
// The hook-spawned worker is DETACHED_PROCESS, so it has no console for a child to inherit and
// Windows gives each console child a fresh, visible one (#143). It sets the one field on the
// existing attr, because the first #143 fix assigned a fresh struct and dropped the flags another
// layer had set. Not CREATE_NO_WINDOW: that would leave codex's interactive `login` no console.
func HideWindow(cmd *exec.Cmd) { ensureSysProcAttr(cmd).HideWindow = true }

// ContainChildren puts THIS process into a fresh kill-on-close job, so every child it starts
// afterwards (`claude -p`, the private `opencode serve`, and their descendants, such as OpenCode
// 2.3.0's pty daemon, which nests its own jobs without breakaway) dies with it, however it dies.
// The first call's result holds for the life of the process — a process can never leave a job —
// which also makes a backfill's in-process worker rerun a no-op. On Windows 8+ the job nests
// inside any job this process already runs in. The handle is unnamed and not inheritable, and
// Go hands a child only the handles it lists (PROC_THREAD_ATTRIBUTE_HANDLE_LIST: its stdio), so
// no child can hold the job open past us.
func (sys) ContainChildren() error {
	containOnce.Do(func() {
		job, err := newWorkerJob()
		if err == nil {
			if err = windows.AssignProcessToJobObject(job, windows.CurrentProcess()); err != nil {
				windows.CloseHandle(job)
				err = fmt.Errorf("AssignProcessToJobObject: %w", err)
			}
		}
		if err != nil {
			containErr = fmt.Errorf("contain the worker's children in a job: %w", err)
			return
		}
		workerJob = job
		contained.Store(true)
	})
	return containErr
}

// newWorkerJob creates an unnamed job with EXACTLY KILL_ON_JOB_CLOSE | BREAKAWAY_OK: the first
// is the point, the second lets Detach's CREATE_BREAKAWAY_FROM_JOB out. Never
// SILENT_BREAKAWAY_OK, which lets every child out unasked and leaves nothing to kill.
func newWorkerJob() (windows.Handle, error) {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return 0, fmt.Errorf("CreateJobObject: %w", err)
	}
	var info windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION
	info.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE | windows.JOB_OBJECT_LIMIT_BREAKAWAY_OK
	if _, err := windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info))); err != nil {
		windows.CloseHandle(job)
		return 0, fmt.Errorf("SetInformationJobObject: %w", err)
	}
	return job, nil
}

// TerminateGroup stops the detached worker on Windows. There is no process-group
// signal equivalent to the Unix SIGTERM-to-(-pid), so we open the process by pid
// and terminate it. os.Process.Kill maps to TerminateProcess. The worker also
// honors the worker_stop_requested meta flag cooperatively; this is the forceful
// backstop the way SIGTERM is on Unix.
//
// TerminateProcess is uncatchable, so the worker's stop context (NotifyStop) never runs
// here, and it does not cascade down the process tree. The worker's children die anyway
// because the worker contained itself (ContainChildren; #218, which absorbed #216): the
// terminated worker's handles close, the last handle to its kill-on-close job with them, and
// the OS kills every member — up to mine_concurrency `claude -p` (whose 20-min timeout is a
// context the dead worker can no longer enforce) and the `opencode serve`, which also ends on
// its stdin lease. A Detach'd wakeup broke away from that job, so it survives, as it must.
func (sys) TerminateGroup(pid int) error {
	p, err := os.FindProcess(pid)
	if err != nil {
		return fmt.Errorf("find worker pid=%d: %w", pid, err)
	}
	if err := p.Kill(); err != nil {
		return fmt.Errorf("terminate worker pid=%d: %w", pid, err)
	}
	return nil
}

// Alive reports whether pid names a live process. Callers ask "is the OWNER of this record
// still running" — the model download lock before stealing it — so failing toward "alive" is
// the safe direction: it declines.
//
// Windows has no signal-0 equivalent, and os.FindProcess is useless as a liveness test here:
// it succeeds for a live pid AND is the very case pid reuse produces. So ask the OS for the
// process's exit code — OpenProcess+GetExitCodeProcess via the syscall package — and treat
// STILL_ACTIVE (259) as alive. An OpenProcess failure returns true only when the reason is
// ACCESS_DENIED (the process exists but belongs to a more privileged account); a genuine
// "no such process" returns false.
func Alive(pid int) bool {
	if pid <= 1 {
		return false
	}
	const stillActive = 259
	h, err := syscall.OpenProcess(syscall.PROCESS_QUERY_INFORMATION|syscall.SYNCHRONIZE, false, uint32(pid))
	if err != nil {
		// ERROR_ACCESS_DENIED means it EXISTS; anything else (typically
		// ERROR_INVALID_PARAMETER for a dead pid) means it does not.
		return errors.Is(err, syscall.ERROR_ACCESS_DENIED)
	}
	defer syscall.CloseHandle(h)
	var code uint32
	if err := syscall.GetExitCodeProcess(h, &code); err != nil {
		return true // handle opened, state unknown: assume alive (declines)
	}
	return code == stillActive
}
