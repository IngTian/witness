package commands

import (
	"context"
	"errors"
	"fmt"
	"io/fs"

	"github.com/IngTian/witness/internal/platform/opencode"
)

const openCodePluginName = opencode.PluginFileName

// opencodeDir is where `wire opencode` writes the plugin and MCP entry. It is OpenCode's own
// lookup, opencode.ConfigDir — the one the auth probe uses too — rather than a copy here: the copy
// that lived here skipped XDG_CONFIG_HOME, so with XDG set the wiring landed where OpenCode never
// reads and still reported success.
//
// This file exists for that one import, the same category as install_codex.go: the engine fence
// (TestEngineHasNoPlatformDispatch) exempts a venue's adapter file by name, not the whole tree.
func opencodeDir() string { return opencode.ConfigDir() }

// openCodeVersion probes the installed OpenCode version. A var so tests never spawn the user's
// real `opencode`.
var openCodeVersion = func(ctx context.Context) (opencode.Version, error) {
	return opencode.ProbeVersion(ctx)
}

// openCodeInstallTarget resolves the install target (shim or self) for wiring. A var so tests on
// Windows do not copy the test binary.
var openCodeInstallTarget = resolveOpenCodeInstall

// checkOpenCodeForWire probes OpenCode and its database before wiring. It returns a slice of
// result rows to print (one per check), or an error that refuses wiring. An absent OpenCode is a
// warning only (capture needs it installed, but wiring continues so it works once installed).
// Unsupported versions and unreadable databases refuse before any write.
func checkOpenCodeForWire(ctx context.Context) (rows []string, err error) {
	v, probeErr := openCodeVersion(ctx)
	if probeErr != nil {
		// OpenCode not found: wire continues so it works once installed
		return []string{
			resultRow(warnGlyph(), "OpenCode", "not found", fmt.Sprintf("capture needs OpenCode %s+", opencode.MinVersion)),
		}, nil
	}

	if err := opencode.CheckVersion(v); err != nil {
		return nil, err
	}

	vStr := v.String()
	if opencode.AboveTested(v) {
		rows = append(rows, resultRow(warnGlyph(), "OpenCode", vStr, fmt.Sprintf("newer than witness has been tested with (%d.%d.x); doctor will say if capture breaks", opencode.NewestTestedMinor.Major, opencode.NewestTestedMinor.Minor)))
	} else {
		rows = append(rows, resultRow(okGlyph(), "OpenCode", vStr, ""))
	}

	// Check database readability (v1-only or mid-migration refuses before writing anything)
	dbPath, err := opencode.DefaultDBPath()
	if err != nil {
		return nil, fmt.Errorf("resolve opencode db path: %w", err)
	}
	_, err = opencode.NewestActivity(ctx, dbPath)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}

	return rows, nil
}
