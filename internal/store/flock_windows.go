//go:build windows

package store

import (
	"os"

	"golang.org/x/sys/windows"
)

// flockFile takes an exclusive, non-blocking lock on f via LockFileEx and returns
// an unlock func that unlocks and closes f. LOCKFILE_FAIL_IMMEDIATELY gives the
// same non-blocking semantics as flock's LOCK_NB: if another process holds the
// lock, LockFileEx returns immediately (ERROR_LOCK_VIOLATION) and ok is false.
// The lock spans the whole file (offset 0, length 0xFFFFFFFF_FFFFFFFF) and is in
// any case released when the handle closes. Owns f either way.
func flockFile(f *os.File) (unlock func(), ok bool) {
	h := windows.Handle(f.Fd())
	ol := &windows.Overlapped{}
	err := windows.LockFileEx(h,
		windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY,
		0, 0xFFFFFFFF, 0xFFFFFFFF, ol)
	if err != nil {
		f.Close()
		return func() {}, false
	}
	return func() {
		_ = windows.UnlockFileEx(h, 0, 0xFFFFFFFF, 0xFFFFFFFF, ol)
		f.Close()
	}, true
}
