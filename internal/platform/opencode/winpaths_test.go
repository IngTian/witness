package opencode

import (
	"strings"
	"testing"

	"github.com/IngTian/witness/internal/platform"
)

// Both builders must share the one implementation, so the Windows rule cannot be fixed in one
// and left broken in the other (they were two identical copies, both wrong). The rule itself is
// pinned on platform.SQLiteFileURI; this is the only test that catches either builder going back
// to url.URL on a non-Windows CI.
func TestBothURIBuildersShareTheSameComposition(t *testing.T) {
	const p = `C:\Users\tzy20\opencode.db`
	if ro, want := readOnlyURI(p), platform.SQLiteFileURI(p, "mode=ro"); ro != want {
		t.Errorf("readOnlyURI diverged: %s vs %s", ro, want)
	}
	if u, want := sqliteURI(p), platform.SQLiteFileURI(p, "mode=ro&cache=shared"); u != want {
		t.Errorf("sqliteURI diverged: %s vs %s", u, want)
	}
	// And neither may reintroduce the url.URL shape.
	for _, got := range []string{readOnlyURI(p), sqliteURI(p)} {
		if strings.Contains(got, "%5C") || strings.HasPrefix(got, "file://") {
			t.Errorf("regressed to the broken Windows shape: %s", got)
		}
	}
}
