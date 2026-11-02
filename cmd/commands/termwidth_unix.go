//go:build !windows

package commands

import (
	"os"

	"golang.org/x/sys/unix"
)

// termWidth is the terminal's column count, or 0 when f is not a terminal or the size is unknown.
func termWidth(f *os.File) int {
	ws, err := unix.IoctlGetWinsize(int(f.Fd()), unix.TIOCGWINSZ)
	if err != nil || ws.Col == 0 {
		return 0
	}
	return int(ws.Col)
}
