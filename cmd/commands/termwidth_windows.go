//go:build windows

package commands

import (
	"os"

	"golang.org/x/sys/windows"
)

// termWidth is the console window's column count, or 0 when f is not a console.
func termWidth(f *os.File) int {
	var info windows.ConsoleScreenBufferInfo
	if err := windows.GetConsoleScreenBufferInfo(windows.Handle(f.Fd()), &info); err != nil {
		return 0
	}
	return int(info.Window.Right - info.Window.Left + 1)
}
