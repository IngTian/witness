package platform

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Every runner child must be built through proc.HideWindow. On Windows the hook-spawned worker is
// DETACHED_PROCESS, so each console child it starts (claude.exe, codex.exe, opencode.exe) opens a
// fresh visible console unless the window is hidden (#143), and only the opencode seam did it. A
// Windows-only test of one seam cannot catch the seam that forgot, so this scans the source on every
// OS (the install_opencode_wintarget_test.go style). One exec.Command* site per adapter keeps the
// scan honest: a second site in a file that already hides would pass the per-file check unhidden.
func TestEveryRunnerSpawnHidesItsWindow(t *testing.T) {
	sites := map[string]int{}
	scanned, total := 0, 0
	err := filepath.WalkDir(".", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return err
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		scanned++
		calls, hides := 0, false
		for _, l := range strings.Split(string(b), "\n") {
			if strings.HasPrefix(strings.TrimSpace(l), "//") {
				continue
			}
			// No paren: a bare `= exec.CommandContext` value is a spawn site too.
			calls += strings.Count(l, "exec.Command")
			hides = hides || strings.Contains(l, "proc.HideWindow(")
		}
		if calls > 0 && !hides {
			t.Errorf("%s builds a child (exec.Command*) without proc.HideWindow", path)
		}
		sites[filepath.Dir(path)] += calls
		total += calls
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for dir, n := range sites {
		if n > 1 {
			t.Errorf("%s: %d exec.Command* sites, want one spawn seam", dir, n)
		}
	}
	if scanned == 0 || total == 0 {
		t.Fatal("scanned no spawn site: the scan is vacuous")
	}
}
