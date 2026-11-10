//go:build windows

package opencode

import (
	"unsafe"

	"golang.org/x/sys/windows"
)

// isOurServePID reports whether pid is a live witness-private `opencode serve`.
//
// Windows has no ppid-orphan sweep (see reapPriorServe), so this pid-file path is the ONLY
// cleanup for a serve whose worker was hard-killed — and therefore the only thing standing
// between a crash and an unbounded pile of servers. It still must not kill on the pid alone,
// because pids are recycled.
//
// It reads the process's own command line and applies the same private-serve fingerprint as the
// Unix sweep (isStrayServeLine), so both platforms agree on what counts as ours; if the OS refuses
// the command line it falls back to the image name (serveProbeMatches). This replaced `wmic`,
// which Windows 11 24H2 removed: the probe then errored for every pid, gate 2 never passed, and
// the reaper reaped nothing while the orphan of every `worker stop` (a TerminateProcess) leaked.
// Both calls here are direct kernel queries, with no child process or service behind them that
// could hang under the WorkerLock, so no timeout is needed. A pid that cannot be opened (gone, or
// another user's) fails CLOSED — no kill.
func isOurServePID(pid int) bool {
	if pid <= 1 {
		return false
	}
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return false
	}
	defer windows.CloseHandle(h)
	return serveProbeMatches(
		func() (string, error) { return processCommandLine(h) },
		func() (string, error) { return processImageName(h) },
	)
}

// processCommandLine returns h's full command line as its creator passed it
// (NtQueryInformationProcess, ProcessCommandLineInformation; Windows 8.1+).
//
// The query writes a UNICODE_STRING header into buf whose Buffer points further into buf. Its
// Length is a uint16 count of BYTES, so no command line exceeds 64 KiB and one fixed buffer always
// fits — no grow-and-retry loop.
func processCommandLine(h windows.Handle) (string, error) {
	buf := make([]byte, unsafe.Sizeof(windows.NTUnicodeString{})+1<<16)
	if err := windows.NtQueryInformationProcess(h, windows.ProcessCommandLineInformation,
		unsafe.Pointer(&buf[0]), uint32(len(buf)), nil); err != nil {
		return "", err
	}
	s := (*windows.NTUnicodeString)(unsafe.Pointer(&buf[0]))
	if s.Buffer == nil {
		return "", nil
	}
	return s.String(), nil
}

// processImageName returns the full Win32 path of h's executable.
func processImageName(h windows.Handle) (string, error) {
	buf := make([]uint16, windows.MAX_LONG_PATH)
	n := uint32(len(buf))
	if err := windows.QueryFullProcessImageName(h, 0, &buf[0], &n); err != nil {
		return "", err
	}
	return windows.UTF16ToString(buf[:n]), nil
}
