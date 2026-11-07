package commands

import (
	"maps"
	"slices"
)

// editor is one wire target: how `witness wire <name>` wires witness into it and how
// `witness unwire <name>` takes it back out. The implementations live in cmd, not in the
// platform subpackages, because wiring is CLI/OS-coupled — settings-file merges, the
// `claude mcp add` shell-out, plugin-JS writing, the GOOS-split hook form — and only cmd
// dispatches on it, so the registry lives here too. wire returns its outcome so the wire
// screen can print its result line last.
//
// This is the WIRE-TARGET axis, distinct from the engine's runner/session axes. (`witness
// install` is a separate command that provisions an archive and wires no editor; the
// pre-v0.5.0 spelling `witness install <target>` describes a CLI that no longer exists.)
type editor struct {
	wire   func() (wireOutcome, error)
	unwire func() (removed bool, warning string, err error)
}

// editors is the wire-target registry: wire/unwire dispatch through it with no target
// switch, their help is built from it, and so is `witness import --agent`'s list — which
// is why "file" (pushed in by `witness ingest`, never imported) is not offered there (#193).
// A test adds a fake target and removes it with t.Cleanup.
var editors = map[string]editor{
	"claude": {wire: cmdInstallClaude, unwire: cmdUninstallClaude},
	"opencode": {wire: cmdInstallOpenCode, unwire: func() (bool, string, error) {
		removed, err := cmdUninstallOpenCode()
		return removed, "", err
	}},
	// Codex wires only hooks: it captures but supplies no RunnerProvider, and its MCP
	// registration lives in a config.toml witness does not own. See install_codex.go.
	"codex": {wire: cmdInstallCodex, unwire: func() (bool, string, error) {
		removed, err := cmdUninstallCodex()
		return removed, "", err
	}},
}

// editorNames is the registered wire targets, SORTED: the wire/unwire Use strings are built
// from it, and ranging the map would reshuffle `witness --help` between runs.
func editorNames() []string { return slices.Sorted(maps.Keys(editors)) }
