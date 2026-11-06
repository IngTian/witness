//go:build !windows

package commands

import (
	"errors"
	"os"
	"os/exec"
	"runtime"
)

// openPath hands path to the desktop's opener and returns once it has STARTED: xdg-open can
// block until the browser exits, and the page needs nothing back from it. Reaped in the
// background so a quick exit leaves no zombie behind for as long as witness runs.
func openPath(path string) error {
	name := browserCommand(runtime.GOOS, os.Getenv)
	if name == "" {
		return errors.New("no desktop display to open a browser on")
	}
	cmd := exec.Command(name, path)
	if err := cmd.Start(); err != nil {
		return err
	}
	go func() { _ = cmd.Wait() }()
	return nil
}

// browserCommand names the program that opens a file in the default browser, or "" when there
// is no desktop: macOS always has `open`; elsewhere xdg-open, but only with an X11 or Wayland
// display — on a headless box it would fall back to a terminal browser or fail.
func browserCommand(goos string, getenv func(string) string) string {
	if goos == "darwin" {
		return "open"
	}
	if getenv("DISPLAY") == "" && getenv("WAYLAND_DISPLAY") == "" {
		return ""
	}
	return "xdg-open"
}
