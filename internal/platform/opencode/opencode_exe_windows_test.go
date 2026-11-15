//go:build windows

package opencode

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// resolveEnv gives ResolveExe only the test's own PATH, USERPROFILE and LOCALAPPDATA. It clears
// WITNESS_OPENCODE_BIN: a run inside an OpenCode session inherits it from the plugin, and it wins
// over every rung these tests exercise.
func resolveEnv(t *testing.T, path, userProfile, localAppData string) {
	t.Helper()
	t.Setenv(openCodeBinEnv, "")
	t.Setenv("PATH", path)
	t.Setenv("USERPROFILE", userProfile)
	t.Setenv("LOCALAPPDATA", localAppData)
}

// ResolveExe's rungs below the plugin binary, one case each. A .cmd shim on PATH resolves to the
// exe it wraps: parsed from an npm (%dp0%) or pnpm (%~dp0) body, else the npm package layout, else
// the shim itself, so spawn errors stay visible. With nothing on PATH, the curl installer's
// %USERPROFILE%\.opencode\bin and then the desktop app's bundled CLI. A desktop-app dir on PATH is
// skipped, so its GUI launcher never wins over a real CLI later on PATH.
//
// Each case's dir is PATH's bin\ plus the home\ (USERPROFILE) and local\ (LOCALAPPDATA) it seeds.
func TestResolveExeWalksItsRungs(t *testing.T) {
	const npmExe = "bin/node_modules/@opencode/cli/bin/opencode.exe"
	const unparseable = "@ECHO off\r\nSOME RANDOM TEXT\r\n"
	shim := func(t *testing.T, dir, body string) string {
		t.Helper()
		p := filepath.Join(dir, "bin", "opencode.cmd")
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T, dir string) (path, want string)
	}{
		{"an npm shim resolves to the real exe", func(t *testing.T, dir string) (string, string) {
			shim(t, dir, "@ECHO off\r\nGOTO start\r\n:find_dp0\r\nSET dp0=%~dp0\r\nEXIT /b\r\n:start\r\nCALL :find_dp0\r\n\"%dp0%\\node_modules\\@opencode\\cli\\bin\\opencode.exe\"   %*\r\n")
			seedFiles(t, dir, npmExe)
			return filepath.Join(dir, "bin"), filepath.Join(dir, npmExe)
		}},
		{"a pnpm-style shim resolves to the real exe", func(t *testing.T, dir string) (string, string) {
			shim(t, dir, "@ECHO off\r\n\"%~dp0\\..\\pnpm\\opencode.exe\" %*\r\n")
			seedFiles(t, dir, "pnpm/opencode.exe")
			return filepath.Join(dir, "bin"), filepath.Join(dir, "pnpm", "opencode.exe")
		}},
		{"an unparseable shim falls back to the npm package layout", func(t *testing.T, dir string) (string, string) {
			shim(t, dir, unparseable)
			seedFiles(t, dir, npmExe)
			return filepath.Join(dir, "bin"), filepath.Join(dir, npmExe)
		}},
		{"an unresolvable shim is kept", func(t *testing.T, dir string) (string, string) {
			return filepath.Join(dir, "bin"), shim(t, dir, unparseable)
		}},
		{"the curl install off PATH", func(t *testing.T, dir string) (string, string) {
			seedFiles(t, dir, "home/.opencode/bin/opencode.exe")
			return "", filepath.Join(dir, "home", ".opencode", "bin", "opencode.exe")
		}},
		{"the desktop app's bundled CLI off PATH", func(t *testing.T, dir string) (string, string) {
			seedFiles(t, dir, "local/Programs/ai.opencode.desktop/resources/opencode-cli.exe")
			return "", filepath.Join(dir, "local", "Programs", "ai.opencode.desktop", "resources", "opencode-cli.exe")
		}},
		{"a desktop dir on PATH is skipped for the CLI after it", func(t *testing.T, dir string) (string, string) {
			seedFiles(t, dir, "Programs/ai.opencode.desktop/opencode.exe", "bin/opencode.exe")
			desktop := filepath.Join(dir, "Programs", "ai.opencode.desktop")
			return desktop + string(filepath.ListSeparator) + filepath.Join(dir, "bin"), filepath.Join(dir, "bin", "opencode.exe")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path, want := tc.setup(t, dir)
			resolveEnv(t, path, filepath.Join(dir, "home"), filepath.Join(dir, "local"))
			got, err := ResolveExe()
			if err != nil {
				t.Fatalf("ResolveExe: %v", err)
			}
			if !strings.EqualFold(got, want) {
				t.Errorf("got %q, want %q", got, want)
			}
		})
	}
}
