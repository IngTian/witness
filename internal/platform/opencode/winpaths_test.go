package opencode

import (
	"context"
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

// `opencode --version` on a packaged Windows build interleaves crash-reporter and startup
// logging with the version, so the old whitespace-field Sscanf parser found NOTHING and witness
// reported "native session isolation unavailable … upgrade to 1.18.0+" against a real 1.18.14.
// Verbatim output from the failing run:
func TestParseOpenCodeVersionHandlesWindowsCrashReporterNoise(t *testing.T) {
	const windowsOut = `17:41:48.929 (crash) > crash reporter started {
  path: 'C:\Users\tzy20\AppData\Roaming\ai.opencode.desktop\Crashpad'
}
17:41:48.947         > app starting { version: '1.18.14', packaged: true, onboardingTest: false }`

	major, minor, patch, ok := parseOpenCodeVersion(windowsOut)
	if !ok {
		t.Fatal("failed to find the version in real Windows output; witness would wrongly claim " +
			"the install is too old and refuse to distill")
	}
	if major != 1 || minor != 18 || patch != 14 {
		t.Errorf("parsed %d.%d.%d, want 1.18.14 (a clock like 17:41:48.929 must NOT be read as a version)",
			major, minor, patch)
	}
}

// The LABELED pattern must be tried before the bare one, and this is the case that proves it.
//
// Found by reverting: with `openCodeVersionLabeled` deleted, every other version test still
// passed — so nothing justified having two patterns, and the labeled one looked like dead
// complexity. It is not. When ANY other version-shaped number precedes the real one, the bare
// pattern picks the wrong value and the 1.18 gate then decides on it:
//
//	"loaded plugin 2.3.4 …  version: '1.18.14'"   bare -> 2.3.4  (passes the gate for the
//	                                               wrong reason — a 2.x plugin, not opencode)
//	"config schema 0.9.1 ok; version: 1.18.14"    bare -> 0.9.1  (FAILS the gate, refusing a
//	                                               perfectly good install)
//
// The second is the dangerous direction: it is exactly the class of false "upgrade to 1.18.0+"
// rejection this whole fix exists to stop.
func TestParseOpenCodeVersionPrefersTheLabelledVersionOverAnEarlierNumber(t *testing.T) {
	for _, tc := range []struct {
		name, in     string
		major, minor int
	}{
		{"a plugin version precedes it",
			"loaded plugin 2.3.4 from disk\napp starting { version: '1.18.14', packaged: true }", 1, 18},
		{"a schema version precedes it",
			"config schema 0.9.1 ok; version: 1.18.14", 1, 18},
		{"a build number precedes it",
			"build 20.1.3\n> app starting { version: '1.19.0' }", 1, 19},
	} {
		t.Run(tc.name, func(t *testing.T) {
			major, minor, _, ok := parseOpenCodeVersion(tc.in)
			if !ok {
				t.Fatal("no version found at all")
			}
			if major != tc.major || minor != tc.minor {
				t.Errorf("parsed %d.%d, want %d.%d — an earlier number was mistaken for the "+
					"version, so the 1.18 gate decides on the wrong value",
					major, minor, tc.major, tc.minor)
			}
		})
	}
}

func TestParseOpenCodeVersionShapes(t *testing.T) {
	for _, tc := range []struct {
		name              string
		in                string
		ok                bool
		major, minor      int
		expectGatePassing bool
	}{
		{"clean unix output", "1.18.13\n", true, 1, 18, true},
		{"v-prefixed", "v1.18.0", true, 1, 18, true},
		{"too old is parsed but must fail the gate", "1.17.9", true, 1, 17, false},
		{"prerelease suffix", "1.18.14-beta.2", true, 1, 18, true},
		{"quoted in a json-ish blob", `{ version: '1.19.0' }`, true, 1, 19, true},
		{"future major", "2.0.0", true, 2, 0, true},
		{"clock timestamps only", "17:41:48.929 starting\n17:41:49.001 ready", false, 0, 0, false},
		{"not found at all", "opencode: command not found", false, 0, 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// The REAL gate, fed this output, so the version→decision mapping is pinned on
			// ValidateOpenCodeCapability itself rather than on a copy of its condition.
			prev := openCodeVersionCommand
			t.Cleanup(func() { openCodeVersionCommand = prev })
			openCodeVersionCommand = func(context.Context) ([]byte, error) { return []byte(tc.in), nil }
			if gate := ValidateOpenCodeCapability(context.Background()) == nil; gate != tc.expectGatePassing {
				t.Errorf("1.18 gate = %v, want %v for %q", gate, tc.expectGatePassing, tc.in)
			}
			major, minor, _, ok := parseOpenCodeVersion(tc.in)
			if ok != tc.ok {
				t.Fatalf("ok = %v, want %v (in=%q)", ok, tc.ok, tc.in)
			}
			if ok && (major != tc.major || minor != tc.minor) {
				t.Errorf("parsed %d.%d, want %d.%d", major, minor, tc.major, tc.minor)
			}
		})
	}
}
