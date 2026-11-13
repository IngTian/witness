package commands

import (
	"maps"
	"slices"

	"github.com/IngTian/witness/internal/store"
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
	health func(st *store.Store, quick bool) []captureCheck
}

// editors is the wire-target registry: wire/unwire dispatch through it with no target
// switch, their help is built from it, and so is `witness import --agent`'s list — which
// is why "file" (pushed in by `witness ingest`, never imported) is not offered there (#193).
// A test adds a fake target and removes it with t.Cleanup.
var editors = map[string]editor{
	"claude":   {wire: cmdInstallClaude, unwire: cmdUninstallClaude},
	"opencode": {wire: cmdInstallOpenCode, unwire: cmdUninstallOpenCode, health: openCodeHealth},
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

// checkLevel is how much one capture check weighs: only checkFail fails doctor's exit code, and
// only checkWarn and checkFail mark doctor's headline and print in status. checkInfo is a state the
// user chose (an editor with sessions they never wired), so it neither warns nor fails.
type checkLevel int

const (
	checkOK checkLevel = iota
	checkInfo
	checkWarn
	checkFail
)

// captureCheck is one row of an editor's capture health, shared by doctor (every row) and status
// (problems only), so both screens word a problem the same way.
type captureCheck struct {
	level      checkLevel
	key, value string    // healthy: the row; otherwise the headline is key + ": " + value
	why        string    // otherwise: the dim explanation
	fix        [2]string // hint label + command; zero = none
}

// editorHealth is one editor's checks under its name: doctor titles a block with it and status
// prefixes each line with it, since "database: unreadable" alone does not say whose database.
type editorHealth struct {
	name   string
	checks []captureCheck
}

// captureHealth returns editor health checks, in editorNames() order, non-empty only.
func captureHealth(st *store.Store, quick bool) []editorHealth {
	var result []editorHealth
	for _, name := range editorNames() {
		ed := editors[name]
		if ed.health == nil {
			continue
		}
		checks := ed.health(st, quick)
		if len(checks) > 0 {
			result = append(result, editorHealth{name: name, checks: checks})
		}
	}
	return result
}
