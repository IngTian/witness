package opencode

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
)

// Reported from a REAL Windows run (issue #10). `witness.exe import --agent opencode` failed with:
//
//	witness: SQL logic error: invalid uri authority:
//	  C:%5CUsers%5Ctzy20%5C.local%5Cshare%5Copencode%5Copencode.db?cache=shared&mode=ro (1)
//
// Both URI builders used url.URL{Scheme:"file", Path: p}, which on a Windows path promotes the
// DRIVE LETTER to the URI authority and percent-encodes every backslash. SQLite rejects it, so
// OpenCode import was impossible on Windows — the one path witness needs to read the user's
// sessions at all.
//
// These run on every OS on purpose: the maintainer develops on macOS and CI is Linux, so a
// runtime.GOOS-gated check would never fire where the bug was introduced.
func TestSQLiteURIHandlesWindowsPaths(t *testing.T) {
	// The exact path from the failing run.
	const win = `C:\Users\tzy20\.local\share\opencode\opencode.db`

	got := sqliteFileURI(win, "mode=ro&cache=shared")
	want := "file:/C:/Users/tzy20/.local/share/opencode/opencode.db?mode=ro&cache=shared"
	if got != want {
		t.Errorf("windows URI wrong:\n got  %s\n want %s", got, want)
	}

	// The three specific defects, asserted individually so a partial regression is legible.
	if strings.Contains(got, "%5C") {
		t.Error("backslashes were percent-encoded; SQLite cannot open that path")
	}
	if strings.HasPrefix(got, "file://") {
		t.Error("the drive letter became the URI AUTHORITY (file://C:...) — the reported failure")
	}
	if !strings.HasPrefix(got, "file:/C:/") {
		t.Errorf("a Windows path needs a leading slash before the drive: %s", got)
	}
}

// Unix paths must be unchanged — this fix must not disturb the platform that works today.
func TestSQLiteURIKeepsUnixPathsWorking(t *testing.T) {
	got := sqliteFileURI("/Users/zetian/.local/share/opencode/opencode.db", "mode=ro")
	want := "file:/Users/zetian/.local/share/opencode/opencode.db?mode=ro"
	if got != want {
		t.Errorf("unix URI changed:\n got  %s\n want %s", got, want)
	}
	// And no query means no trailing '?'.
	if bare := sqliteFileURI("/tmp/a.db", ""); bare != "file:/tmp/a.db" {
		t.Errorf("empty query should emit no '?': %s", bare)
	}
}

// '?' and '#' MUST be escaped: they terminate the path, so an unescaped one opens a DIFFERENT
// (usually empty) database — silently reading the wrong file instead of failing. My first
// version of this fix regressed exactly this, caught by the pre-existing
// TestReadOnlyURIConnectsEscapedAbsolutePathReadOnly, which uses a `witness#archive.db` path.
func TestSQLiteURIEscapesPathCharsThatWouldTruncateIt(t *testing.T) {
	for _, tc := range []struct{ in, wantSub string }{
		{`/tmp/witness#archive.db`, "%23"},
		{`/tmp/witness?x.db`, "%3F"},
		{`/tmp/already%20escaped.db`, "%2520"}, // a literal % must be escaped FIRST
		{`C:\dir\a#b?c.db`, "%23"},
	} {
		got := sqliteFileURI(tc.in, "mode=ro")
		if !strings.Contains(got, tc.wantSub) {
			t.Errorf("sqliteFileURI(%q) = %s; want it to contain %s", tc.in, got, tc.wantSub)
		}
		// The query must remain the LAST '?' — i.e. exactly one unescaped '?' in the URI.
		if n := strings.Count(got, "?"); n != 1 {
			t.Errorf("sqliteFileURI(%q) = %s has %d unescaped '?'; the path must not add one", tc.in, got, n)
		}
	}
}

// End-to-end: a path with every hostile character still opens the RIGHT database and still
// refuses writes. This is the guarantee that keeps distillation off the user's own opencode.db.
func TestSQLiteURIOpensTheRightDatabaseReadOnly(t *testing.T) {
	for _, name := range []string{
		"plain.db",
		"witness#archive.db",
		"has space.db",
		"pct%20name.db",
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), name)
			w, err := sql.Open("sqlite", path)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := w.Exec(`CREATE TABLE marker(v TEXT); INSERT INTO marker VALUES ('right db')`); err != nil {
				t.Fatal(err)
			}
			w.Close()

			ro, err := sql.Open("sqlite", readOnlyURI(path))
			if err != nil {
				t.Fatalf("open %q: %v", name, err)
			}
			defer ro.Close()
			var v string
			if err := ro.QueryRow(`SELECT v FROM marker`).Scan(&v); err != nil || v != "right db" {
				t.Fatalf("opened the WRONG database for %q: v=%q err=%v", name, v, err)
			}
			if _, err := ro.Exec(`INSERT INTO marker VALUES ('nope')`); err == nil {
				t.Errorf("mode=ro allowed a write for %q — the read-only guarantee is broken", name)
			}
		})
	}
}

// A Windows path must NOT be run through filepath.Abs on a non-Windows host.
//
// filepath.Abs is GOOS-dependent: off Windows it does not consider `C:\dir\file` absolute and
// prepends the current directory, yielding `file:/<cwd>/C:/dir/file` — pointing at a path that
// does not exist. My first version of this fix had exactly that bug, and this is what caught it.
func TestSQLiteURIDoesNotPrefixAnAlreadyAbsoluteWindowsPath(t *testing.T) {
	got := sqliteFileURI(`C:\dir\file.db`, "")
	if got != "file:/C:/dir/file.db" {
		t.Errorf("got %s; a drive-letter path is already absolute and must not gain the cwd", got)
	}
	// UNC form is absolute too.
	if unc := sqliteFileURI(`\\server\share\a.db`, ""); !strings.HasPrefix(unc, "file://server/share/") {
		t.Errorf("UNC path mangled: %s", unc)
	}
}

// A RELATIVE path must still be resolved, since callers may pass one.
func TestSQLiteURIResolvesRelativePaths(t *testing.T) {
	got := sqliteFileURI("relative.db", "")
	if !strings.HasPrefix(got, "file:/") {
		t.Errorf("relative path was not made absolute: %s", got)
	}
	if strings.Contains(got, "file:/relative.db") {
		t.Errorf("relative path was treated as absolute: %s", got)
	}
}

func TestIsAbsolutePathAcrossConventions(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want bool
	}{
		{"/unix/abs", true},
		{`C:\win\abs`, true},
		{"c:/win/lower", true},
		{`\\server\share`, true},
		{"relative/path", false},
		{"", false},
		{"C:", false}, // drive with no separator is not a usable absolute path
		{"1:/notaletter", false},
		{"./rel", false},
	} {
		if got := isAbsolutePath(tc.in); got != tc.want {
			t.Errorf("isAbsolutePath(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

// Both builders must share the one implementation, so the Windows rule cannot be fixed in one
// and left broken in the other (they were two identical copies, both wrong).
func TestBothURIBuildersShareTheSameComposition(t *testing.T) {
	const p = `C:\Users\tzy20\opencode.db`
	if ro, want := readOnlyURI(p), sqliteFileURI(p, "mode=ro"); ro != want {
		t.Errorf("readOnlyURI diverged: %s vs %s", ro, want)
	}
	if u, want := sqliteURI(p), sqliteFileURI(p, "mode=ro&cache=shared"); u != want {
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
			major, minor, _, ok := parseOpenCodeVersion(tc.in)
			if ok != tc.ok {
				t.Fatalf("ok = %v, want %v (in=%q)", ok, tc.ok, tc.in)
			}
			if !ok {
				return
			}
			if major != tc.major || minor != tc.minor {
				t.Errorf("parsed %d.%d, want %d.%d", major, minor, tc.major, tc.minor)
			}
			// Mirror ValidateOpenCodeCapability's gate so the version→decision mapping is pinned.
			gate := !(major < 1 || (major == 1 && minor < 18))
			if gate != tc.expectGatePassing {
				t.Errorf("1.18 gate = %v, want %v for %q", gate, tc.expectGatePassing, tc.in)
			}
		})
	}
}
