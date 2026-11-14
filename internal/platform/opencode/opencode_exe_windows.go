//go:build windows

package opencode

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

const openCodeBinEnv = "WITNESS_OPENCODE_BIN"

// desktopMarkers identify directories belonging to the OpenCode DESKTOP app rather than the
// headless CLI. A match means "skip this PATH entry".
//
// v2 desktop installs put nothing on PATH, so the markers change no outcome under v2. Under v1
// the desktop put a GUI launcher named `opencode` on PATH, and resolving to that meant the GUI
// opened instead of the headless CLI running — which is how a machine with both installed
// reported "native session isolation unavailable" while a perfectly good CLI sat later on PATH.
// witness only ever runs non-interactive subcommands (`serve --stdio`, `models`, `export`,
// `--version`), so a GUI is a failure.
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

// pluginExe returns the plugin-supplied binary when WITNESS_OPENCODE_BIN names an existing
// regular file. A missing path or a directory returns ("", false), so resolution falls through.
func pluginExe() (string, bool) {
	path := os.Getenv(openCodeBinEnv)
	if path == "" {
		return "", false
	}
	info, err := os.Stat(path)
	if err != nil || info.IsDir() {
		return "", false
	}
	return path, true
}

var shimTargetRe = regexp.MustCompile(`(?i)"%~?dp0%?\\([^"]+?\.exe)"`)

// cmdShimTarget attempts to resolve a `.cmd` or `.bat` shim (how npm/pnpm/yarn install CLIs
// on Windows) to the real exe it wraps. It first tries to parse the shim body for a line
// like `"%dp0%\node_modules\@opencode\cli\bin\opencode.exe"` (npm) or
// `"%~dp0\..\pnpm\opencode.exe"` (pnpm), joining the extracted relative path to the shim's
// directory. If that fails, it falls back to the standard npm package layout:
// `<dir>\node_modules\@opencode\cli\bin\opencode.exe`. Either result must exist as a file.
//
// If neither approach yields an existing file, it returns ("", false), and the caller returns
// the shim itself rather than failing, so that spawn errors remain visible.
func cmdShimTarget(shim string) (string, bool) {
	body, err := os.ReadFile(shim)
	if err != nil {
		return "", false
	}
	dir := filepath.Dir(shim)
	// Try parsing the shim body for the target path
	if m := shimTargetRe.FindSubmatch(body); m != nil {
		rel := string(m[1])
		target := filepath.Clean(filepath.Join(dir, rel))
		if info, err := os.Stat(target); err == nil && !info.IsDir() {
			return target, true
		}
	}
	// Fall back to the npm package layout
	fallback := filepath.Join(dir, "node_modules", "@opencode", "cli", "bin", "opencode.exe")
	if info, err := os.Stat(fallback); err == nil && !info.IsDir() {
		return fallback, true
	}
	return "", false
}

// ResolveExe resolves the `opencode` CLI to an absolute path, preferring the headless CLI
// over the desktop app's GUI launcher.
//
// The plugin-supplied binary is checked first, because the plugin knows where OpenCode actually
// runs from, which beats a stale PATH entry. Then it walks PATH in order (the same precedence
// the OS would use), skipping desktop-launcher directories, and honours PATHEXT so a `.cmd`/`.bat`
// wrapper — how npm-installed CLIs usually land on Windows — is found, not just a bare `.exe`.
// When a `.cmd` or `.bat` shim is found, it attempts to resolve it to the real exe (npm/pnpm/yarn
// all use shims with parseable targets). If PATH yields nothing, it checks the curl installer's
// canonical path (%USERPROFILE%\.opencode\bin\opencode.exe) and the desktop app's bundled CLI.
// Finally it falls back to exec.LookPath, which restores the default behaviour rather than
// inventing a failure.
//
// Note: `bun install -g @opencode/cli` (per docs) puts a launcher named `opencode.exe` on PATH,
// plus an `opencode.bunx` file. That launcher is unverified and is returned as-is. The lease
// passes through a launcher that holds only the read end of stdin, so the serve cannot be
// orphaned, and the plugin's binary wins anyway.
func ResolveExe() (string, error) {
	// 1. Plugin-supplied binary
	if path, ok := pluginExe(); ok {
		return path, nil
	}

	// 2. PATH walk, skipping desktop dirs, resolving shims
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
			// If it's a .cmd or .bat shim, try to resolve it to the real exe
			if strings.EqualFold(ext, ".cmd") || strings.EqualFold(ext, ".bat") {
				if target, ok := cmdShimTarget(candidate); ok {
					return target, nil
				}
				// Unresolvable shim: return it anyway, so spawn errors are visible
				return candidate, nil
			}
			return candidate, nil
		}
	}

	// 3. curl installer's canonical path
	if homeDir, err := os.UserHomeDir(); err == nil {
		curlExe := filepath.Join(homeDir, ".opencode", "bin", "opencode.exe")
		if info, err := os.Stat(curlExe); err == nil && !info.IsDir() {
			return curlExe, nil
		}
	}

	// 4. Desktop app's bundled CLI
	if localAppData := os.Getenv("LOCALAPPDATA"); localAppData != "" {
		desktopCLI := filepath.Join(localAppData, "Programs", "ai.opencode.desktop", "resources", "opencode-cli.exe")
		if info, err := os.Stat(desktopCLI); err == nil && !info.IsDir() {
			return desktopCLI, nil
		}
	}

	// 5. Fall back to exec.LookPath
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
