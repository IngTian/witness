package commands

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/IngTian/witness/internal/store"
)

// Command-layer regressions from the adversarial review of `witness merge`. Each failed on the
// first implementation.

// The dry run must not promise a registration the apply then refuses. That disagreement kept a
// re-run from ever converging: every --apply took another full-archive backup.
func TestReviewRefusedLensIsReportedAndDoesNotRetakeBackups(t *testing.T) {
	export := mergeFixture(t, func(st *store.Store) {
		if err := st.RegisterLens("math", writeLensDir(t, "math", "mine math", "fold math")); err != nil {
			t.Fatal(err)
		}
	})
	// A lens definition registration will refuse: its mining prompt is gone.
	if err := os.Remove(filepath.Join(export, "lenses", "math", "extract.md")); err != nil {
		t.Fatal(err)
	}
	root := openBase(t).Root

	out := captureStdout(t, func() { _ = cmdMerge(export, false) })
	if strings.Contains(out, "math: will be registered") || !strings.Contains(out, "would be refused") {
		t.Errorf("the dry run promised a registration the apply will refuse:\n%s", out)
	}
	_ = captureStdout(t, func() { _ = cmdMerge(export, true) })
	_ = captureStdout(t, func() { _ = cmdMerge(export, true) })
	if n := len(premergeBackups(t, root)); n != 1 {
		t.Errorf("re-running a merge with nothing new took %d backups; want 1", n)
	}
}

// WITNESS_HOME with a trailing slash (a shell tab-completes it) put the backup INSIDE the archive,
// and replacing the root as the undo instructions say deleted the backup with it.
func TestReviewBackupIsASiblingEvenWithATrailingSlash(t *testing.T) {
	export := mergeFixture(t, nil)
	root := openBase(t).Root
	t.Setenv("WITNESS_HOME", root+string(os.PathSeparator))
	_ = captureStdout(t, func() {
		if err := cmdMerge(export, true); err != nil {
			t.Fatal(err)
		}
	})
	entries, _ := os.ReadDir(root)
	for _, e := range entries {
		if strings.Contains(e.Name(), "premerge") {
			t.Errorf("the backup %s is INSIDE the archive; replacing the root would delete it", e.Name())
		}
	}
	if len(premergeBackups(t, filepath.Clean(root))) != 1 {
		t.Error("want exactly one backup beside the archive")
	}
}

// A single .db carries no lens definitions. Its data still arrives, and nothing reviews a lens that
// is not registered here — so the report must say so instead of counting the rows "to review".
func TestReviewDataForAnUndefinedLensIsReported(t *testing.T) {
	t.Setenv(noDefaultLensEnv, "1")
	t.Setenv("WITNESS_HOME", t.TempDir())
	inc, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	if err := inc.AppendObservations([]store.Observation{{ID: "m1", Session: "s1", Lens: "math", Dimension: "d",
		Observation: "x", Source: "mined", TS: "2026-03-01T00:00:00Z"}}); err != nil {
		t.Fatal(err)
	}
	db := filepath.Join(t.TempDir(), "plain.db")
	if err := inc.Export(db, false); err != nil {
		t.Fatal(err)
	}
	inc.Close()
	t.Setenv("WITNESS_HOME", t.TempDir())
	openBase(t)

	out := captureStdout(t, func() { _ = cmdMerge(db, false) })
	if !strings.Contains(out, "math: its data arrives, but there is no definition for it here") {
		t.Errorf("data for an undefined lens must be reported:\n%s", out)
	}
}

// The printed command to adopt their portrait prompt must actually work: <root>/summarize/
// normally does not exist, so a bare cp failed.
func TestReviewPortraitPromptCommandCreatesTheDirectory(t *testing.T) {
	export := mergeFixture(t, func(st *store.Store) {
		if err := os.MkdirAll(filepath.Join(st.Root, "summarize"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(st.Root, "summarize", "unified.md"), []byte("PORTRAIT"), 0o600); err != nil {
			t.Fatal(err)
		}
	})
	openBase(t)
	out := captureStdout(t, func() { _ = cmdMerge(export, false) })
	if !strings.Contains(out, "mkdir -p ") || !strings.Contains(out, `cp "`) {
		t.Errorf("the printed command must create the directory and quote its paths:\n%s", out)
	}
}

// The backup path is a SIBLING of the archive however the root is spelled. Pinned directly because
// the store also cleans WITNESS_HOME, which would mask a regression here.
func TestReviewBackupPathIsNeverInsideTheArchive(t *testing.T) {
	root := filepath.Join(t.TempDir(), "arch")
	p, err := premergeBackupPath(root+string(os.PathSeparator), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Dir(p) != filepath.Dir(root) {
		t.Errorf("backup %s is not beside the archive %s", p, root)
	}
}
