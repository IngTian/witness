//go:build windows

package opencode

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestResolveExeResolvesAnNpmCmdShimToTheRealExe verifies that when witness finds an npm
// `opencode.cmd` shim, it parses the shim's body to extract the real exe path and returns
// that exe instead of the shim itself.
func TestResolveExeResolvesAnNpmCmdShimToTheRealExe(t *testing.T) {
	dir := t.TempDir()
	shim := filepath.Join(dir, "opencode.cmd")
	target := filepath.Join(dir, "node_modules", "@opencode", "cli", "bin", "opencode.exe")
	os.MkdirAll(filepath.Dir(target), 0o755)
	os.WriteFile(target, nil, 0o644)
	// npm cmd-shim body with %dp0% expansion
	shimBody := "@ECHO off\r\nGOTO start\r\n:find_dp0\r\nSET dp0=%~dp0\r\nEXIT /b\r\n:start\r\nCALL :find_dp0\r\n\"%dp0%\\node_modules\\@opencode\\cli\\bin\\opencode.exe\"   %*\r\n"
	os.WriteFile(shim, []byte(shimBody), 0o644)

	t.Setenv("PATH", dir)
	t.Setenv("USERPROFILE", t.TempDir())
	t.Setenv("LOCALAPPDATA", t.TempDir())

	got, err := ResolveExe()
	if err != nil {
		t.Fatalf("ResolveExe: %v", err)
	}
	if !strings.EqualFold(got, target) {
		t.Errorf("got %q, want the real exe %q", got, target)
	}
}

// TestResolveExeResolvesAPnpmStyleShim verifies that pnpm-style shims (using %~dp0 instead
// of %dp0%) are also parsed correctly.
func TestResolveExeResolvesAPnpmStyleShim(t *testing.T) {
	dir := t.TempDir()
	shim := filepath.Join(dir, "opencode.cmd")
	target := filepath.Join(dir, "..", "pnpm", "opencode.exe")
	os.MkdirAll(filepath.Dir(filepath.Join(dir, target)), 0o755)
	targetAbs := filepath.Clean(filepath.Join(dir, "..", "pnpm", "opencode.exe"))
	os.MkdirAll(filepath.Dir(targetAbs), 0o755)
	os.WriteFile(targetAbs, nil, 0o644)
	// pnpm uses %~dp0
	shimBody := "@ECHO off\r\n\"%~dp0\\..\\pnpm\\opencode.exe\" %*\r\n"
	os.WriteFile(shim, []byte(shimBody), 0o644)

	t.Setenv("PATH", dir)
	t.Setenv("USERPROFILE", t.TempDir())
	t.Setenv("LOCALAPPDATA", t.TempDir())

	got, err := ResolveExe()
	if err != nil {
		t.Fatalf("ResolveExe: %v", err)
	}
	if !strings.EqualFold(got, targetAbs) {
		t.Errorf("got %q, want the real exe %q", got, targetAbs)
	}
}

// TestResolveExeFallsBackToTheNpmPackageLayout verifies that when a shim body is unparseable,
// witness still tries the standard npm package layout relative to the shim.
func TestResolveExeFallsBackToTheNpmPackageLayout(t *testing.T) {
	dir := t.TempDir()
	shim := filepath.Join(dir, "opencode.cmd")
	target := filepath.Join(dir, "node_modules", "@opencode", "cli", "bin", "opencode.exe")
	os.MkdirAll(filepath.Dir(target), 0o755)
	os.WriteFile(target, nil, 0o644)
	// unparseable shim body
	shimBody := "@ECHO off\r\nSOME RANDOM TEXT\r\n"
	os.WriteFile(shim, []byte(shimBody), 0o644)

	t.Setenv("PATH", dir)
	t.Setenv("USERPROFILE", t.TempDir())
	t.Setenv("LOCALAPPDATA", t.TempDir())

	got, err := ResolveExe()
	if err != nil {
		t.Fatalf("ResolveExe: %v", err)
	}
	if !strings.EqualFold(got, target) {
		t.Errorf("got %q, want the fallback npm layout %q", got, target)
	}
}

// TestResolveExeKeepsAnUnresolvableShim verifies that when a shim cannot be resolved
// (neither parsing works nor the fallback layout exists), witness returns the shim itself
// rather than failing, so that spawn errors remain visible.
func TestResolveExeKeepsAnUnresolvableShim(t *testing.T) {
	dir := t.TempDir()
	shim := filepath.Join(dir, "opencode.cmd")
	// unparseable shim body, and no fallback exe
	shimBody := "@ECHO off\r\nSOME RANDOM TEXT\r\n"
	os.WriteFile(shim, []byte(shimBody), 0o644)

	t.Setenv("PATH", dir)
	t.Setenv("USERPROFILE", t.TempDir())
	t.Setenv("LOCALAPPDATA", t.TempDir())

	got, err := ResolveExe()
	if err != nil {
		t.Fatalf("ResolveExe: %v", err)
	}
	if !strings.EqualFold(got, shim) {
		t.Errorf("got %q, want the unresolvable shim %q", got, shim)
	}
}

// TestResolveExeFindsTheCurlInstallOffPath verifies that when PATH is empty or has no
// opencode, witness checks %USERPROFILE%\.opencode\bin\opencode.exe (the curl installer's
// canonical path).
func TestResolveExeFindsTheCurlInstallOffPath(t *testing.T) {
	userProfile := t.TempDir()
	curlBin := filepath.Join(userProfile, ".opencode", "bin", "opencode.exe")
	os.MkdirAll(filepath.Dir(curlBin), 0o755)
	os.WriteFile(curlBin, nil, 0o644)

	t.Setenv("PATH", "")
	t.Setenv("USERPROFILE", userProfile)
	t.Setenv("LOCALAPPDATA", t.TempDir())

	got, err := ResolveExe()
	if err != nil {
		t.Fatalf("ResolveExe: %v", err)
	}
	if !strings.EqualFold(got, curlBin) {
		t.Errorf("got %q, want the curl install %q", got, curlBin)
	}
}

// TestResolveExeFindsTheDesktopCLI verifies that when no other install is found, witness
// checks %LOCALAPPDATA%\Programs\ai.opencode.desktop\resources\opencode-cli.exe (the desktop
// app's bundled CLI).
func TestResolveExeFindsTheDesktopCLI(t *testing.T) {
	localAppData := t.TempDir()
	desktopCLI := filepath.Join(localAppData, "Programs", "ai.opencode.desktop", "resources", "opencode-cli.exe")
	os.MkdirAll(filepath.Dir(desktopCLI), 0o755)
	os.WriteFile(desktopCLI, nil, 0o644)

	t.Setenv("PATH", "")
	t.Setenv("USERPROFILE", t.TempDir())
	t.Setenv("LOCALAPPDATA", localAppData)

	got, err := ResolveExe()
	if err != nil {
		t.Fatalf("ResolveExe: %v", err)
	}
	if !strings.EqualFold(got, desktopCLI) {
		t.Errorf("got %q, want the desktop CLI %q", got, desktopCLI)
	}
}

// TestResolveExeSkipsDesktopDirsOnPath verifies that PATH entries containing desktop markers
// are skipped, so the GUI launcher is never returned when a real CLI exists later on PATH.
func TestResolveExeSkipsDesktopDirsOnPath(t *testing.T) {
	desktopDir := filepath.Join(t.TempDir(), "Programs", "ai.opencode.desktop")
	os.MkdirAll(desktopDir, 0o755)
	desktopExe := filepath.Join(desktopDir, "opencode.exe")
	os.WriteFile(desktopExe, nil, 0o644)

	cliDir := t.TempDir()
	cliExe := filepath.Join(cliDir, "opencode.exe")
	os.WriteFile(cliExe, nil, 0o644)

	// desktop dir first, cli dir second
	t.Setenv("PATH", desktopDir+string(filepath.ListSeparator)+cliDir)
	t.Setenv("USERPROFILE", t.TempDir())
	t.Setenv("LOCALAPPDATA", t.TempDir())

	got, err := ResolveExe()
	if err != nil {
		t.Fatalf("ResolveExe: %v", err)
	}
	if !strings.EqualFold(got, cliExe) {
		t.Errorf("got %q, want the CLI %q (desktop dir should be skipped)", got, cliExe)
	}
}
