//go:build opencodecontract && windows

package opencode

import (
	"errors"
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"
)

// procPIDs lists the live processes whose image name is name (procName-normalized), from one
// Toolhelp32 snapshot. Not WMIC (gone from windows-latest) nor tasklist (a subprocess per poll).
func procPIDs(t *testing.T, name string) []int {
	t.Helper()
	snap, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		t.Fatalf("CreateToolhelp32Snapshot: %v", err)
	}
	defer windows.CloseHandle(snap)
	want := procName(name)
	var pids []int
	e := windows.ProcessEntry32{Size: uint32(unsafe.Sizeof(windows.ProcessEntry32{}))}
	for err = windows.Process32First(snap, &e); err == nil; err = windows.Process32Next(snap, &e) {
		if procName(windows.UTF16ToString(e.ExeFile[:])) == want {
			pids = append(pids, int(e.ProcessID))
		}
	}
	if !errors.Is(err, windows.ERROR_NO_MORE_FILES) {
		t.Fatalf("Process32Next: %v", err)
	}
	return pids
}
