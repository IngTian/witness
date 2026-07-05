//go:build windows

package commands

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// installRoot is where witness installs itself on Windows: %LOCALAPPDATA%\witness.
// LOCALAPPDATA is the per-user, NON-roaming app-data dir — the correct home for a
// large machine-specific payload (the ~448MB embedding model must not sync across
// a roaming profile). Mirrors winget's %LOCALAPPDATA%\Microsoft\WinGet\Packages.
// Falls back to %USERPROFILE%\AppData\Local if LOCALAPPDATA is somehow unset.
func installRoot() (string, error) {
	if d := os.Getenv("LOCALAPPDATA"); d != "" {
		return filepath.Join(d, "witness"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "AppData", "Local", "witness"), nil
}

// resolveClaudeInstall (Windows) makes the install self-contained: a Windows
// Claude Code hook runs in exec form (no shell), so it cannot use the bash shim.
// Instead we COPY the binary and its bundled assets/prompts into a stable install
// dir and register an exec-form hook pointing at the installed witness.exe. This
// is the "never run from a throwaway build checkout" rule (rustup/winget copy on
// install); bundle.Dir then self-locates assets beside the installed exe with no
// env var, and root.go already enforces the recursion guard, so no shim jobs are
// lost. Idempotent: re-running overwrites the install and re-merges the hooks.
func resolveClaudeInstall() (hookInvocation, error) {
	root, err := installRoot()
	if err != nil {
		return hookInvocation{}, err
	}
	exe, err := installBundle(root)
	if err != nil {
		return hookInvocation{}, err
	}
	fmt.Printf("witness installed to %s\n", root)
	// Put the install dir on the user PATH so `witness` runs from any new shell.
	// Best-effort: the hooks use the absolute exe path and work regardless, so a
	// PATH failure only affects the interactive `witness` command.
	if err := ensureOnUserPath(root); err != nil {
		fmt.Fprintf(os.Stderr, "witness: could not update PATH (add %s manually): %v\n", root, err)
	}
	return execInvocation(exe), nil
}

// installBundle copies the running binary and its sibling assets/ and prompts/
// trees into root, returning the path to the installed witness.exe. The source
// layout is resolved via bundle.Dir semantics (assets/prompts sit beside the exe
// in an installed layout, or one level up in a build checkout). Missing prompts
// is fatal (distillation needs them); a missing model is only warned about — the
// capture hooks work without it, and the user can fetch/copy the model later.
func installBundle(root string) (string, error) {
	srcExe, err := os.Executable()
	if err != nil {
		return "", err
	}
	if resolved, err := filepath.EvalSymlinks(srcExe); err == nil {
		srcExe = resolved
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return "", err
	}

	dstExe := filepath.Join(root, "witness.exe")
	if err := copyFile(srcExe, dstExe); err != nil {
		// The most likely cause on a re-install is that the installed witness.exe
		// is a running image (the MCP server or a background worker holds Claude
		// Code's session), so Windows denies replacing it. Point the user at the
		// fix rather than surfacing a raw sharing-violation error.
		return "", fmt.Errorf("could not update %s (close Claude Code, then re-run install): %w", dstExe, err)
	}

	// Copy the bundled prompts/ and assets/ (the model) that ship beside the exe
	// in the release zip. The binary resolves both at runtime via bundle.Dir's
	// exe-relative probe, so copying them next to the installed exe is what makes
	// the install self-contained. prompts/ is required (distillation reads it); a
	// missing model is only a warning (capture works without it, and it can be
	// dropped in later). Source layout: siblings of the exe (zip layout) or one
	// level up (a build checkout).
	srcDir := filepath.Dir(srcExe)
	if src, ok := probeSrcTree(srcDir, "prompts"); ok {
		if err := copyTree(src, filepath.Join(root, "prompts")); err != nil {
			return "", fmt.Errorf("copy prompts: %w", err)
		}
	} else {
		return "", fmt.Errorf("prompts/ not found near %s; run from the unzipped release folder (or a built checkout)", srcExe)
	}
	if src, ok := probeSrcTree(srcDir, filepath.Join("assets", "e5-small")); ok {
		if err := copyTree(src, filepath.Join(root, "assets", "e5-small")); err != nil {
			return "", fmt.Errorf("copy model: %w", err)
		}
	} else {
		fmt.Fprintf(os.Stderr, "witness: embedding model not found near %s; "+
			"capture will work, but distillation needs the model — "+
			"place it in %s\\assets\\e5-small (or set WITNESS_ASSETS) later\n",
			srcExe, root)
	}
	return dstExe, nil
}

// probeSrcTree finds a bundled subtree relative to the source exe dir, matching
// bundle.Dir's two layouts (sibling in an installed tree, parent in a build
// checkout). Returns the first that exists.
func probeSrcTree(exeDir, subdir string) (string, bool) {
	for _, cand := range []string{
		filepath.Join(exeDir, subdir),
		filepath.Join(filepath.Dir(exeDir), subdir),
	} {
		if _, err := os.Stat(cand); err == nil {
			return cand, true
		}
	}
	return "", false
}

// copyFile copies src to dst (0o755 so the binary stays executable), atomically:
// it streams to a temp file on the same directory and renames it over dst, so an
// interrupted copy can never replace a good file with a truncated one (mirrors
// writeFileAtomic for settings.json). If src and dst are the SAME file (e.g. a
// re-install run from the already-installed exe now that install put its dir on
// PATH), it is a no-op — Windows would otherwise deny writing a running image.
func copyFile(src, dst string) error {
	if si, err := os.Stat(src); err == nil {
		if di, err := os.Stat(dst); err == nil && os.SameFile(si, di) {
			return nil // src IS dst; nothing to copy (and can't overwrite a live exe)
		}
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	tmp := dst + ".tmp"
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		os.Remove(tmp)
		return err
	}
	if err := out.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	// Rename is atomic on the same volume. On Windows, os.Rename fails if dst is a
	// running image; surface that as a clear "close Claude Code" hint upstream.
	if err := os.Rename(tmp, dst); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// copyTree recursively copies the directory src to dst. Used for the small
// prompts/ tree and the (large but flat) assets/e5-small model dir.
func copyTree(src, dst string) error {
	return filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if info.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		return copyFile(path, target)
	})
}
