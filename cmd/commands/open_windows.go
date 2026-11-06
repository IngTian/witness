package commands

import "golang.org/x/sys/windows"

// openPath opens path with its associated program (the default browser for .html) through
// ShellExecute: no shell and no `start` command line, so a path with spaces or & needs no
// quoting.
func openPath(path string) error {
	verb, err := windows.UTF16PtrFromString("open")
	if err != nil {
		return err
	}
	file, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	return windows.ShellExecute(0, verb, file, nil, nil, windows.SW_SHOWNORMAL)
}
