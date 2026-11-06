//go:build windows

package commands

import (
	"errors"
	"fmt"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

// ensureOnUserPath idempotently adds dir to the CURRENT USER's PATH (Windows), so
// `witness` is runnable from any new shell after install. It edits the per-user
// environment in the registry (HKCU\Environment) — the same mechanism rustup,
// Scoop, and winget use — and broadcasts WM_SETTINGCHANGE so already-running
// Explorer/GUI shells pick up the change without a logout.
//
// It deliberately does NOT use `setx`: setx truncates PATH at 1024 chars (silent
// data loss on real machines) and flattens %VAR% references. We instead read the
// raw value with GetStringValue (which does NOT expand %VAR%) and write it back
// with SetExpandStringValue (REG_EXPAND_SZ), so tokens like %SystemRoot% survive
// verbatim. Best-effort and non-fatal: a failure here only means the user must
// add dir to PATH manually; the hooks (absolute exe path) work regardless.
//
// Unix has no counterpart: its install points hooks at the in-repo shim by absolute path and
// never relocates the binary, so there is nothing to add to PATH, and silently editing shell rc
// files is exactly the surprise to avoid.
func ensureOnUserPath(dir string) error {
	k, err := registry.OpenKey(registry.CURRENT_USER, "Environment", registry.QUERY_VALUE|registry.SET_VALUE)
	if err != nil {
		return fmt.Errorf("open HKCU\\Environment: %w", err)
	}
	defer k.Close()

	cur, valtype, err := k.GetStringValue("Path")
	switch {
	case errors.Is(err, registry.ErrNotExist):
		cur, valtype = "", registry.EXPAND_SZ // no user PATH yet; create one
	case errors.Is(err, registry.ErrUnexpectedType):
		// A non-string PATH is corrupt/hostile; refuse to touch it (rustup does
		// the same) rather than risk clobbering something we don't understand.
		return fmt.Errorf("HKCU\\Environment\\Path has unexpected type %d; not modifying", valtype)
	case err != nil:
		return fmt.Errorf("read HKCU\\Environment\\Path: %w", err)
	}

	next, changed := appendToPathValue(cur, dir)
	if !changed {
		fmt.Printf("%s already on your user PATH\n", dir)
		return nil
	}
	// Preserve EXPAND_SZ if the value already used it (so existing %VAR% tokens
	// keep expanding); default to EXPAND_SZ for a freshly created value too.
	if valtype == registry.SZ {
		if err := k.SetStringValue("Path", next); err != nil {
			return fmt.Errorf("write PATH: %w", err)
		}
	} else if err := k.SetExpandStringValue("Path", next); err != nil {
		return fmt.Errorf("write PATH: %w", err)
	}

	broadcastEnvChange()
	fmt.Printf("added %s to your user PATH (open a new terminal to use `witness`)\n", dir)
	return nil
}

// broadcastEnvChange notifies running processes that the environment changed, so
// Explorer-spawned shells inherit the new PATH without a logout. Uses
// SendMessageTimeoutW(HWND_BROADCAST, WM_SETTINGCHANGE, 0, "Environment", ...)
// via a lazily-loaded user32 proc — pure syscall, no cgo. Best-effort: the PATH
// edit is already persisted in the registry, so a broadcast failure only delays
// pickup until the next new shell.
func broadcastEnvChange() {
	const (
		hwndBroadcast   = 0xffff // HWND_BROADCAST
		wmSettingChange = 0x001a // WM_SETTINGCHANGE
		smtoAbortIfHung = 0x0002 // SMTO_ABORTIFHUNG
	)
	env, err := windows.UTF16PtrFromString("Environment")
	if err != nil {
		return
	}
	user32 := windows.NewLazySystemDLL("user32.dll")
	proc := user32.NewProc("SendMessageTimeoutW")
	if err := proc.Find(); err != nil {
		return // user32 unavailable (e.g. a stripped/headless SKU) — skip quietly
	}
	var result uintptr
	proc.Call(
		uintptr(hwndBroadcast),
		uintptr(wmSettingChange),
		0,
		uintptr(unsafe.Pointer(env)),
		uintptr(smtoAbortIfHung),
		uintptr(5000), // 5s timeout, matching rustup/Scoop
		uintptr(unsafe.Pointer(&result)),
	)
}
