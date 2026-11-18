//go:build windows

package claude

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// On PATH, npm's install is claude.cmd; newClaudeCmd must run the claude.exe behind it, hidden,
// and refuse the shim once that exe is gone, never fall back to cmd.exe.
func TestNewClaudeCmdRunsTheExeBehindNpmsShim(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "node_modules", "@anthropic-ai", "claude-code", "bin", "claude.exe")
	if err := os.MkdirAll(filepath.Dir(exe), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{exe, filepath.Join(dir, "claude.cmd")} {
		if err := os.WriteFile(f, []byte("@echo off\r\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", dir)

	cmd := newClaudeCmd(context.Background(), "")
	if cmd.Err != nil || !strings.EqualFold(cmd.Path, exe) {
		t.Fatalf("newClaudeCmd = %q, %v; want the npm layout's %q", cmd.Path, cmd.Err, exe)
	}
	if cmd.SysProcAttr == nil || !cmd.SysProcAttr.HideWindow {
		t.Error("the claude child must be built with proc.HideWindow")
	}
	if err := os.Remove(exe); err != nil {
		t.Fatal(err)
	}
	if newClaudeCmd(context.Background(), "").Err == nil {
		t.Error("shim without layout must be refused")
	}
}
