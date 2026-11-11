// Package bundle resolves the directories holding witness's bundled runtime
// assets — the embedding model (assets/e5-small) and the prompt/lens templates
// (prompts/). These ship ALONGSIDE the binary, not in the per-user data dir, so
// their location depends on how witness was launched, not on WITNESS_HOME.
//
// This exists because the Unix hook shim (hooks/witness.sh) used to be the only
// thing that located the assets: it exports CLAUDE_PLUGIN_ROOT computed from its
// own path. A Windows exec-form hook has no shell to run the shim, so the binary
// must self-locate. Dir() adds an exe-relative probe that works with no env at
// all, which is strictly better than the old CWD-relative fallback (that only
// worked when the process happened to start in the repo root).
package bundle

import (
	"os"
	"path/filepath"
)

// Dir resolves the on-disk directory for a bundled asset subtree. subdir is the
// path under the asset root (e.g. "prompts" or filepath.Join("assets",
// "e5-small")). envOverride, when non-empty, names an env var checked first that
// points DIRECTLY at the resolved dir (e.g. WITNESS_PROMPTS, WITNESS_ASSETS).
//
// Resolution order, first hit wins:
//  1. $envOverride, if set — an explicit, fully-resolved override.
//  2. $CLAUDE_PLUGIN_ROOT/subdir — exported by the Unix hook shim
//     (hooks/witness.sh) as its checkout root, for a working-copy install. The
//     name is Claude Code's plugin variable; witness no longer ships a plugin, but
//     the shim and these lookups still agree on it.
//  3. exe-relative: the first of <exeDir>/subdir or <exeDir>/../subdir that
//     exists. This covers both an installed layout (witness + assets/ siblings)
//     and the build layout (bin/witness-os-arch under the repo, assets/ one up).
//     Windows exec-form hooks rely on this — nothing sets CLAUDE_PLUGIN_ROOT.
//  4. subdir, CWD-relative — the last-resort dev fallback (preserves the old
//     behavior so error messages/tests that ran from the repo root still work).
func Dir(subdir, envOverride string) string {
	if envOverride != "" {
		if d := os.Getenv(envOverride); d != "" {
			return d
		}
	}
	if root := os.Getenv("CLAUDE_PLUGIN_ROOT"); root != "" {
		return filepath.Join(root, subdir)
	}
	if d, ok := exeRelative(subdir); ok {
		return d
	}
	return subdir
}

// exeRelative resolves the running binary (following symlinks so a PATH-symlinked
// install still finds its real siblings) and probes for subdir near it. ok is
// false if the executable can't be determined or neither candidate exists — the
// caller then falls back, so behavior is unchanged when assets aren't laid out
// this way.
func exeRelative(subdir string) (string, bool) {
	dir, ok := exeDir()
	if !ok {
		return "", false
	}
	return probeFrom(dir, subdir)
}

func exeDir() (string, bool) {
	exe, err := os.Executable()
	if err != nil {
		return "", false
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	return filepath.Dir(exe), true
}

// CreateDir is where to CREATE subdir: Dir's answer, except for its cwd-relative fallback. Dir
// can only probe for a directory that already exists, so before the first `witness model fetch`
// it falls back to the cwd, and the model would land wherever the user happened to stand. Here
// the bundle root is instead found by prompts/, which every layout ships beside the binary, so
// the new dir appears exactly where Dir's exe-relative probe will look next time. With no
// prompts/ near the binary (`go run`), the cwd fallback stands.
func CreateDir(subdir, envOverride string) string {
	d := Dir(subdir, envOverride)
	if d != subdir {
		return d
	}
	if dir, ok := exeDir(); ok {
		return createFrom(dir, subdir)
	}
	return subdir
}

func createFrom(exeDir, subdir string) string {
	if prompts, ok := probeFrom(exeDir, "prompts"); ok {
		return filepath.Join(filepath.Dir(prompts), subdir)
	}
	return subdir
}

// probeFrom returns the first of the two supported layouts under exeDir that
// exists on disk: a sibling (installed: <exeDir>/witness + <exeDir>/subdir) or a
// parent (build: <repo>/bin/witness-* + <repo>/subdir). Split out from exeRelative
// so the layout logic is unit-testable without relocating the test binary.
func probeFrom(exeDir, subdir string) (string, bool) {
	for _, cand := range []string{
		filepath.Join(exeDir, subdir),               // installed: <dir>/witness + <dir>/subdir
		filepath.Join(filepath.Dir(exeDir), subdir), // build: <repo>/bin/witness-* + <repo>/subdir
	} {
		if _, err := os.Stat(cand); err == nil {
			return cand, true
		}
	}
	return "", false
}
