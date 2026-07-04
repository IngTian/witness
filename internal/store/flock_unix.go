//go:build !windows

package store

import (
	"os"
	"syscall"
)

// flockFile takes an exclusive, non-blocking advisory lock on f (flock(2)) and
// returns an unlock func that releases the lock and closes f. If the lock is held
// by another process, ok is false and f is closed. Owns f either way.
func flockFile(f *os.File) (unlock func(), ok bool) {
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return func() {}, false
	}
	return func() {
		syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		f.Close()
	}, true
}
