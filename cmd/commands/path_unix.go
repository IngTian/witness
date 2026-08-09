//go:build !windows

package commands

// ensureOnUserPath is a no-op on Unix. The Unix install points hooks at the
// in-repo shim by absolute path and does not relocate the binary, so there is
// nothing to add to PATH; and silently editing ~/.profile / shell rc files is
// exactly the kind of surprise the research said to avoid. If a user wants
// `witness` on PATH they symlink it into ~/.local/bin themselves. Windows is
// different (see path_windows.go): install copies the exe into %LOCALAPPDATA%
// \witness, which is not on PATH by default, so it must register it.
//
// DELIBERATELY UNREFERENCED ON UNIX — both `deadcode` and `staticcheck U1000` report it as unused,
// and both are right about this file and wrong about the program: the sole caller is
// install_windows.go, which the Unix build never compiles. Deleting it would break the Windows build,
// so it is the ONE expected finding from either tool. Anything else they report is real.
func ensureOnUserPath(dir string) error { return nil }
