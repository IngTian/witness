package proc

import (
	"slices"
	"strings"
	"testing"
)

func TestPsField(t *testing.T) {
	cases := []struct {
		line     string
		wantN    int
		wantRest string
		wantOK   bool
	}{
		{"  4321   1 opencode serve --pure", 4321, "1 opencode serve --pure", true},
		{"1 opencode serve", 1, "opencode serve", true},
		{"\t99\tclaude -p", 99, "claude -p", true},
		{"notanumber rest", 0, "", false},
		{"", 0, "", false},
		{"12345", 0, "", false}, // no command column
	}
	for _, tc := range cases {
		n, rest, ok := psField(tc.line)
		if ok != tc.wantOK || n != tc.wantN || rest != tc.wantRest {
			t.Fatalf("psField(%q) = (%d, %q, %v), want (%d, %q, %v)", tc.line, n, rest, ok, tc.wantN, tc.wantRest, tc.wantOK)
		}
	}
}

// TestOrphanPIDs is the core safety test for the reap primitive: only an ORPHANED
// process (ppid==1) whose command satisfies the predicate may be selected. A live
// sibling (ppid!=1 — still owned by its launcher), a non-matching process, the
// scanner's own pid, and init itself must all be skipped. The predicate here stands
// in for a real fingerprint (opencode owns its own; see TestIsStrayServeLine there).
func TestOrphanPIDs(t *testing.T) {
	const self = 700
	want := func(cmdline string) bool { return strings.Contains(cmdline, "MATCH") }
	psOut := strings.Join([]string{
		"  4321     1 /bin/thing MATCH orphan",   // orphan + match → reap
		"  4400  4399 /bin/thing MATCH sibling",  // LIVE sibling (ppid!=1) → skip
		"  4500     1 /bin/other no-fingerprint", // orphan but no match → skip
		"   700     1 /bin/thing MATCH self",     // self → skip
		"     1     0 /sbin/launchd",             // init → skip
		"  garbage line that does not parse",     // skip
	}, "\n")

	got := OrphanPIDs(psOut, self, want)
	if wantPIDs := []int{4321}; !slices.Equal(got, wantPIDs) {
		t.Fatalf("OrphanPIDs = %v, want %v", got, wantPIDs)
	}
}

// TestSysGracefulStopNilIsNoop confirms the real controller treats a nil process as a
// no-op (never panics), matching the callers that hold an optional child handle.
func TestSysGracefulStopNilIsNoop(t *testing.T) {
	if err := System().GracefulStop(nil); err != nil {
		t.Fatalf("GracefulStop(nil) = %v, want nil", err)
	}
}
