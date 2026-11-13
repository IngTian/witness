//go:build !windows

package store

import "os"

// renameReplace is os.Rename: on Unix replacing a file is atomic and never refused because
// another writer has it open, so there is nothing to retry (see rename_windows.go).
func renameReplace(from, to string) error { return os.Rename(from, to) }
