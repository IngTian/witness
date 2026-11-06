package platform

import (
	"path/filepath"
	"strings"
)

// SQLiteFileURI is the one place a SQLite `file:` URI is composed, for every source that
// opens its own native database (OpenCode, Codex). query is appended verbatim
// (already-encoded k=v&k=v): callers pass fixed internal values such as "mode=ro", so
// there is nothing to escape.
//
// It must NOT be built with url.URL{Scheme:"file", Path: p}. On Windows that produced
// `file://C:%5CUsers%5C...`: the drive letter is parsed as the URI AUTHORITY and every
// backslash is percent-encoded, so SQLite rejects it outright. Reported from a real Windows
// run (issue #10):
//
//	witness.exe import --agent opencode
//	witness: SQL logic error: invalid uri authority:
//	  C:%5CUsers%5Ctzy20%5C.local%5Cshare%5Copencode%5Copencode.db?cache=shared&mode=ro (1)
//
// A SQLite file: URI wants FORWARD slashes and a leading `/` before the drive letter
// (`file:/C:/Users/...`), and the path must not be percent-escaped. filepath.ToSlash is NOT
// usable: it is a no-op on non-Windows, so a Windows path handled on any other GOOS would keep
// its backslashes. The replacement is unconditional on purpose, which also makes this
// testable off Windows.
//
// The path needs SELECTIVE escaping, which is the subtlety that makes hand-rolling this
// dangerous in both directions:
//   - '?' and '#' MUST be escaped. They terminate the path (query / fragment), so a real data
//     dir like `witness#archive.db` would silently open a DIFFERENT, empty database — reading
//     the wrong file rather than failing. (A regression test covers exactly that path.)
//   - '%' MUST be escaped first, or an existing percent in a filename would be re-read as an
//     escape sequence.
//   - ':' and '/' must NOT be escaped: the drive colon in `C:/…` and the separators are
//     structural. This is precisely where url.URL got it wrong — it percent-encoded the
//     backslashes and promoted `C:` to an authority.
func SQLiteFileURI(path, query string) string {
	// Only resolve a RELATIVE path. filepath.Abs is GOOS-dependent: on non-Windows it does not
	// recognize `C:\dir\file` as absolute and prepends the current directory, producing
	// `file:/<cwd>/C:/dir/file`. That matters for more than tests — it is why this rule must be
	// explicit rather than delegated — and it keeps the function verifiable off Windows.
	if !isAbsolutePath(path) {
		if abs, err := filepath.Abs(path); err == nil {
			path = abs
		}
	}
	p := strings.ReplaceAll(path, `\`, "/")
	// Order matters: '%' first, so the escapes introduced below are not double-escaped.
	p = strings.ReplaceAll(p, "%", "%25")
	p = strings.ReplaceAll(p, "?", "%3F")
	p = strings.ReplaceAll(p, "#", "%23")
	if !strings.HasPrefix(p, "/") {
		p = "/" + p // a Windows path starts at the drive letter: C:/... -> /C:/...
	}
	uri := "file:" + p
	if query != "" {
		uri += "?" + query
	}
	return uri
}

// isAbsolutePath recognizes an absolute path in EITHER convention, regardless of the running
// GOOS: a leading separator (`/foo`, `\\server\share`) or a drive letter (`C:\foo`, `c:/foo`).
// filepath.IsAbs only understands the host's own convention, which is not enough here — the
// same composition must behave identically wherever it runs.
func isAbsolutePath(p string) bool {
	if p == "" {
		return false
	}
	if p[0] == '/' || p[0] == '\\' {
		return true
	}
	// Drive-letter form: exactly one ASCII letter, a colon, then a separator.
	if len(p) >= 3 && p[1] == ':' && (p[2] == '\\' || p[2] == '/') {
		c := p[0] | 0x20 // lowercase
		return c >= 'a' && c <= 'z'
	}
	return false
}
