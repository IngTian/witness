//go:build windows

package opencode

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// desktopShimMarker identifies the OpenCode DESKTOP app's bundled launcher directory.
//
// Installing the desktop app puts a launcher named `opencode` on PATH under a
// `@opencode-aidesktop` package dir. That launcher starts the GUI; it is not the headless
// CLI. witness only ever runs non-interactive subcommands (`serve --pure`, `models`,
// `session export/import`), so resolving to the desktop launcher means the GUI opens (or the
// process exits with output witness cannot parse) instead of the command running — which is
// how a machine with both installed reported "native session isolation unavailable" while a
// perfectly good CLI sat later on the same PATH.
// desktopMarkers identify directories belonging to the OpenCode DESKTOP app rather than the
// headless CLI. A match means "skip this PATH entry".
//
// More than one spelling is needed, from the Windows box's own reports: the npm-scoped package dir
// is `@opencode-aidesktop`, while the app's own data lives under `ai.opencode.desktop` (its crash
// reporter writes to ...\AppData\Roaming\ai.opencode.desktop\Crashpad). A single marker would
// skip one layout and happily return the GUI launcher from the other.
var desktopMarkers = []string{"@opencode-aidesktop", "ai.opencode.desktop", "opencode.desktop"}

// isDesktopDir reports whether a PATH entry belongs to the desktop app.
func isDesktopDir(dir string) bool {
	lower := strings.ToLower(filepath.ToSlash(dir))
	for _, m := range desktopMarkers {
		if strings.Contains(lower, m) {
			return true
		}
	}
	return false
}

// openCodeExe resolves the `opencode` CLI to an absolute path, preferring the headless CLI
// over the desktop app's GUI launcher.
//
// It walks PATH in order (the same precedence the OS would use), skipping desktop-launcher
// directories, and honours PATHEXT so a `.cmd`/`.bat` wrapper — how npm-installed CLIs
// usually land on Windows — is found, not just a bare `.exe`. If nothing matches it falls
// back to exec.LookPath, which restores the default behaviour rather than inventing a
// failure: that matters when the ONLY install is the desktop one, where running its launcher
// and reporting whatever it says beats refusing to try.
func openCodeExe() (string, error) {
	exts := pathExtensions()
	for _, dir := range filepath.SplitList(os.Getenv("PATH")) {
		if dir == "" {
			continue
		}
		dir = filepath.Clean(dir)
		if isDesktopDir(dir) {
			continue
		}
		for _, ext := range exts {
			candidate := filepath.Join(dir, "opencode"+ext)
			info, err := os.Stat(candidate)
			if err != nil || info.IsDir() {
				continue
			}
			// Windows os.Stat is CASE-INSENSITIVE, so this candidate can match a desktop
			// `OpenCode.exe` sitting in a directory whose name carries no desktop marker. Check the
			// name the filesystem actually reports: the headless CLI is lowercase `opencode`, the
			// desktop build ships `OpenCode.exe`. Skipping on that difference is what stops witness
			// spawning a GUI that can never serve — the failure that produced `context deadline
			// exceeded` on every distillation until the exe was pinned.
			if actual := info.Name(); actual != "" && !strings.HasPrefix(actual, "opencode") {
				continue
			}
			return candidate, nil
		}
	}
	return exec.LookPath("opencode")
}

// pathExtensions returns the executable suffixes to try, from PATHEXT, always including the
// ones an OpenCode install actually uses. PATHEXT is normally
// ".COM;.EXE;.BAT;.CMD;…" but can be missing or trimmed in a stripped environment (a service
// account, a bare CI container), so the defaults are unioned in rather than assumed.
func pathExtensions() []string {
	out := []string{".exe", ".cmd", ".bat"}
	seen := map[string]bool{".exe": true, ".cmd": true, ".bat": true}
	for _, e := range filepath.SplitList(os.Getenv("PATHEXT")) {
		e = strings.ToLower(strings.TrimSpace(e))
		if e == "" || seen[e] {
			continue
		}
		seen[e] = true
		out = append(out, e)
	}
	return out
}
