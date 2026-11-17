package opencode

import (
	"strings"
	"testing"

	"github.com/IngTian/witness/internal/platform"
)

// sqliteURI must compose through the one shared implementation, so the Windows rule cannot be
// fixed there and left broken here. The rule itself is pinned on platform.SQLiteFileURI; this is
// the only test that catches sqliteURI going back to url.URL on a non-Windows CI.
func TestSQLiteURIUsesTheSharedComposition(t *testing.T) {
	const p = `C:\Users\tzy20\opencode.db`
	got := sqliteURI(p)
	if want := platform.SQLiteFileURI(p, "mode=ro&cache=shared"); got != want {
		t.Errorf("sqliteURI diverged: %s vs %s", got, want)
	}
	if strings.Contains(got, "%5C") || strings.HasPrefix(got, "file://") {
		t.Errorf("regressed to the broken Windows shape: %s", got)
	}
}
