//go:build opencodecontract && windows

package opencode

import (
	"errors"
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"
)

// procList maps every live pid to the procName of its image name, from one Toolhelp32 snapshot.
// Not WMIC (gone from windows-latest) nor tasklist (a subprocess per poll).
func procList(t *testing.T) map[int]string {
	t.Helper()
	snap, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		t.Fatalf("CreateToolhelp32Snapshot: %v", err)
	}
	defer windows.CloseHandle(snap)
	procs := map[int]string{}
	e := windows.ProcessEntry32{Size: uint32(unsafe.Sizeof(windows.ProcessEntry32{}))}
	for err = windows.Process32First(snap, &e); err == nil; err = windows.Process32Next(snap, &e) {
		procs[int(e.ProcessID)] = procName(windows.UTF16ToString(e.ExeFile[:]))
	}
	if !errors.Is(err, windows.ERROR_NO_MORE_FILES) {
		t.Fatalf("Process32Next: %v", err)
	}
	return procs
}
