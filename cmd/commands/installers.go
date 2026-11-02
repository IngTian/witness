package commands

import "github.com/IngTian/witness/internal/platform"

// Concrete install-target adapters. They live here (not in the platform
// subpackages) because installation is CLI/OS-coupled — settings-file merges, the
// `claude mcp add` shell-out, plugin-JS writing, the GOOS-split hook form. The
// platform.Installer interface lets the install/uninstall commands dispatch through
// one registry (no target switch) and lets the fake-3rd-platform proof test
// register its own, while keeping the machinery in the command layer where it
// belongs. wire/unwire (wirer/unwirer, install.go) are the same calls with the
// outcome kept, so the wire screen can print its result line last.
func init() {
	platform.RegisterInstaller("claude", claudeInstaller{})
	platform.RegisterInstaller("opencode", openCodeInstaller{})
	platform.RegisterInstaller("codex", codexInstaller{})
}

type claudeInstaller struct{}

func (claudeInstaller) Install() error                    { _, err := cmdInstallClaude(); return err }
func (claudeInstaller) Uninstall() error                  { _, err := cmdUninstallClaude(); return err }
func (claudeInstaller) wire() (wireOutcome, error)        { return cmdInstallClaude() }
func (claudeInstaller) unwire() (removed bool, err error) { return cmdUninstallClaude() }

type openCodeInstaller struct{}

func (openCodeInstaller) Install() error                    { _, err := cmdInstallOpenCode(); return err }
func (openCodeInstaller) Uninstall() error                  { _, err := cmdUninstallOpenCode(); return err }
func (openCodeInstaller) wire() (wireOutcome, error)        { return cmdInstallOpenCode() }
func (openCodeInstaller) unwire() (removed bool, err error) { return cmdUninstallOpenCode() }

// codexInstaller wires only hooks: Codex captures but supplies no RunnerProvider, and
// its MCP registration lives in a config.toml witness does not own. See install_codex.go.
type codexInstaller struct{}

func (codexInstaller) Install() error                    { _, err := cmdInstallCodex(); return err }
func (codexInstaller) Uninstall() error                  { _, err := cmdUninstallCodex(); return err }
func (codexInstaller) wire() (wireOutcome, error)        { return cmdInstallCodex() }
func (codexInstaller) unwire() (removed bool, err error) { return cmdUninstallCodex() }
