//go:build windows

package opencode

import (
	"errors"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
)

// isOurServePID reports whether pid is a live witness-private `opencode serve`.
//
// Windows has no ppid-orphan sweep (see reapPriorServe), so this pid-file path is the ONLY
// cleanup for a serve whose worker was hard-killed — and therefore the only thing standing
// between a crash and an unbounded pile of servers. It still must not kill on the pid alone,
// because pids are recycled.
//
// Corroboration uses WMIC's CommandLine for exactly that pid and applies the same
// private-serve fingerprint as the Unix sweep (isStrayServeLine), so both platforms agree on
// what counts as ours. Notes on the mechanics:
//   - `wmic process where processid=N get commandline` prints a "CommandLine" header plus the
//     value; we hand the whole output to the fingerprint, which scans fields, so the header is
//     harmless. If the process is gone WMIC prints "No Instance(s) Available." and the
//     fingerprint rejects it.
//   - WMIC is deprecated on Windows 11+ but still present; PowerShell would be the successor
//     and costs ~100x the startup. A missing WMIC yields an error here, which fails CLOSED
//     (no kill) — the deliberate choice, since a leaked serve is recoverable and killing the
//     user's editor is not.
//   - The command runs with a hidden window like every other opencode-adjacent spawn.
func isOurServePID(pid int) bool {
	cmd := exec.Command("wmic", "process", "where", "processid="+strconv.Itoa(pid), "get", "commandline")
	hideOpenCodeWindow(cmd)
	out, err := cmd.Output()
	if err != nil {
		return false
	}
	text := string(out)
	if strings.Contains(text, "No Instance(s) Available") {
		return false
	}
	return isStrayServeLine(text)
}

// processAlive reports whether pid names a live process.
//
// Windows has no signal-0 equivalent, and os.FindProcess is useless as a liveness test here:
// it succeeds for a live pid AND is the very case pid reuse produces. So ask the OS for the
// process's exit code — OpenProcess+GetExitCodeProcess via the syscall package — and treat
// STILL_ACTIVE (259) as alive.
//
// It answers "is the OWNER still running", so failing toward "alive" is the safe direction: it
// makes reapPriorServe decline to kill. An OpenProcess failure therefore returns true only
// when the reason is ACCESS_DENIED (the process exists but belongs to a more privileged
// account); a genuine "no such process" returns false.
func processAlive(pid int) bool {
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
		return true // handle opened, state unknown: assume alive (declines the kill)
	}
	return code == stillActive
}
