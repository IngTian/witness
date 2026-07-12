package platform

// Installer wires witness INTO a target runtime (hooks/plugin + MCP registration)
// and removes it again. Unlike the other capabilities, its concrete impls live in
// cmd/commands, NOT in the platform subpackages: installation is inherently
// CLI/OS-coupled (settings-file merges, `claude mcp add` shelling, plugin-JS
// writing, the GOOS-split hook form) — none of which belongs in a runtime adapter.
// So the interface lives here (for a single registry-driven dispatch + the proof
// test), but cmd registers the concrete installers at startup.
//
// This is the install-TARGET axis (a user typing `witness install claude`), which
// is distinct from and never touches the engine's runner/session axes.
type Installer interface {
	// Install wires witness into the target runtime, idempotently.
	Install() error
	// Uninstall removes witness's integration, idempotently. Data/config/runner are
	// left untouched (uninstall is not "reset").
	Uninstall() error
}

// installers is the install-target registry, populated by cmd via RegisterInstaller
// (separate from the Platform registry because the impls are cmd-side). Reads
// happen after startup registration, so no lock is needed.
var installers = map[string]Installer{}

// RegisterInstaller records the installer for a target name (e.g. "claude"). cmd
// calls this once per target at startup. Panics on empty/duplicate — a wiring bug
// caught immediately, not a runtime condition.
func RegisterInstaller(name string, in Installer) {
	if name == "" {
		panic("platform: RegisterInstaller called with empty name")
	}
	if _, dup := installers[name]; dup {
		panic("platform: duplicate installer registration for " + name)
	}
	installers[name] = in
}

// InstallerFor returns the installer for a target name, or (nil, false) if the
// target is unknown — the caller fails closed with a clear message rather than
// silently doing nothing.
func InstallerFor(name string) (Installer, bool) {
	in, ok := installers[name]
	return in, ok
}

// InstallTargets returns the registered target names (for usage/help text), so the
// command's error message stays in sync with what's actually installable.
func InstallTargets() []string {
	out := make([]string, 0, len(installers))
	for name := range installers {
		out = append(out, name)
	}
	return out
}
