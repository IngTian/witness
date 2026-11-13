//go:build windows

package store

import (
	"errors"
	"os"
	"syscall"
	"time"

	"golang.org/x/sys/windows"
)

// renameReplace is os.Rename with a bounded retry for Windows' transient refusals. Replacing a
// file (MoveFileEx with MOVEFILE_REPLACE_EXISTING) fails with ERROR_ACCESS_DENIED or
// ERROR_SHARING_VIOLATION while another writer holds the target open or is mid-replace of it,
// which is exactly what two concurrent writers of one config.toml produce: on windows-latest,
// 10 of 12 concurrent writeAtomic calls failed outright (#220). The window is milliseconds, so
// retrying turns the collision back into the last-writer-wins Unix gives. cmd/go's robustio
// retries the same errors for the same reason. A target that stays refused (a read-only file)
// still fails, after the deadline.
func renameReplace(from, to string) error {
	delay := time.Millisecond
	deadline := time.Now().Add(2 * time.Second)
	for {
		err := os.Rename(from, to)
		if err == nil || !transientRenameError(err) || time.Now().After(deadline) {
			return err
		}
		time.Sleep(delay)
		if delay < 64*time.Millisecond {
			delay *= 2
		}
	}
}

func transientRenameError(err error) bool {
	var errno syscall.Errno
	return errors.As(err, &errno) && (errno == windows.ERROR_ACCESS_DENIED || errno == windows.ERROR_SHARING_VIOLATION)
}
