//go:build !windows

package commands

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/IngTian/witness/internal/store"
)

// A lens that fails to register AFTER the data committed must make the command fail. It used to
// exit 0 with "[ok] merged", so `witness merge x --apply && rm -rf x` deleted the only copy of a
// prompt that exists in no database. Unix-only because it holds the registry's flock directly.
func TestReviewFailedLensRegistrationExitsNonZero(t *testing.T) {
	export := mergeFixture(t, func(st *store.Store) {
		if err := st.RegisterLens("math", writeLensDir(t, "math", "mine math", "fold math")); err != nil {
			t.Fatal(err)
		}
	})
	root := openBase(t).Root
	f, err := os.OpenFile(filepath.Join(root, ".lens-registry.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	defer syscall.Flock(int(f.Fd()), syscall.LOCK_UN)

	var mergeErr error
	out := captureStdout(t, func() { mergeErr = cmdMerge(export, true) })
	if mergeErr == nil || !strings.Contains(mergeErr.Error(), "NOT registered") {
		t.Errorf("a lens that failed to register must fail the command, got %v", mergeErr)
	}
	if strings.Contains(out, "[ok] merged") || strings.Contains(out, "✓ merged") {
		t.Errorf("the run must not report a clean success:\n%s", out)
	}
}
