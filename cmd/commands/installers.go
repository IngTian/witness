package commands

import "github.com/IngTian/witness/internal/platform"

// Concrete install-target adapters. They live here (not in the platform
// subpackages) because installation is CLI/OS-coupled — settings-file merges, the
// `claude mcp add` shell-out, plugin-JS writing, the GOOS-split hook form. The
// platform.Installer interface lets the install/uninstall commands dispatch through
// one registry (no target switch) and lets the fake-3rd-platform proof test
// register its own, while keeping the machinery in the command layer where it
// belongs.
func init() {
	platform.RegisterInstaller("claude", claudeInstaller{})
	platform.RegisterInstaller("opencode", openCodeInstaller{})
}

type claudeInstaller struct{}

func (claudeInstaller) Install() error   { return cmdInstallClaude() }
func (claudeInstaller) Uninstall() error { return cmdUninstallClaude() }

type openCodeInstaller struct{}

func (openCodeInstaller) Install() error   { return cmdInstallOpenCode() }
func (openCodeInstaller) Uninstall() error { return cmdUninstallOpenCode() }
