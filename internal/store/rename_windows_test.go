//go:build windows

package store

import (
	"errors"
	"os"
	"testing"

	"golang.org/x/sys/windows"
)

// Only the two refusals a concurrent replace produces are retried; anything else (a missing
// source, a bad path) fails at once rather than spinning for two seconds.
func TestTransientRenameErrorIsOnlyTheReplaceRefusals(t *testing.T) {
	link := func(errno error) error { return &os.LinkError{Op: "rename", Old: "a", New: "b", Err: errno} }
	for _, tc := range []struct {
		err  error
		want bool
	}{
		{link(windows.ERROR_ACCESS_DENIED), true},
		{link(windows.ERROR_SHARING_VIOLATION), true},
		{link(windows.ERROR_FILE_NOT_FOUND), false},
		{link(windows.ERROR_PATH_NOT_FOUND), false},
		{errors.New("plain"), false},
	} {
		if got := transientRenameError(tc.err); got != tc.want {
			t.Errorf("transientRenameError(%v) = %v, want %v", tc.err, got, tc.want)
		}
	}
}
