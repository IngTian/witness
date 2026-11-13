package commands

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"time"

	"github.com/IngTian/witness/internal/platform/opencode"
	"github.com/IngTian/witness/internal/platform/opencode/plugin"
	"github.com/IngTian/witness/internal/store"
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

// openCodeHealth checks OpenCode capture status for doctor and status. Returns nil (silent)
// when OpenCode is not in use. quick skips version and plugin checks (for status).
func openCodeHealth(st *store.Store, quick bool) []captureCheck {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Gate: silent unless OpenCode is in use
	wired := opencode.PluginInstalled()
	seen := opencode.LastImported(st)
	dbPath, dbPathErr := opencode.DefaultDBPath()
	var newest time.Time
	var dbErr error
	if dbPathErr == nil {
		newest, dbErr = opencode.NewestActivity(ctx, dbPath)
	}

	inUse := false
	if quick {
		inUse = wired || !seen.IsZero()
	} else {
		inUse = wired || !seen.IsZero() || (dbErr == nil && !newest.IsZero())
	}
	if !inUse {
		return nil
	}

	var checks []captureCheck

	// Full path only: version, plugin, MCP checks
	if !quick {
		// Version check
		v, verr := openCodeVersion(ctx)
		if verr != nil {
			checks = append(checks, captureCheck{
				level: checkWarn,
				key:   "version",
				value: "cannot probe",
				why:   fmt.Sprintf("witness expects OpenCode %s+ but cannot check: %v", opencode.MinVersion, verr),
			})
		} else {
			if cerr := opencode.CheckVersion(v); cerr != nil {
				vStr := v.String()
				msg := cerr.Error()
				fix := [2]string{}
				if errors.Is(cerr, opencode.ErrOpenCodeV1) {
					fix = [2]string{"upgrade", "opencode upgrade --major"}
				} else if errors.Is(cerr, opencode.ErrOpenCodeTooOld) {
					fix = [2]string{"upgrade", "opencode upgrade"}
				}
				checks = append(checks, captureCheck{
					level: checkFail,
					key:   "version",
					value: vStr,
					why:   msg,
					fix:   fix,
				})
			} else if opencode.AboveTested(v) {
				vStr := v.String()
				checks = append(checks, captureCheck{
					level: checkWarn,
					key:   "version",
					value: vStr,
					why:   fmt.Sprintf("newer than witness has been tested with (%d.%d.x)", opencode.NewestTestedMinor.Major, opencode.NewestTestedMinor.Minor),
				})
			} else {
				checks = append(checks, captureCheck{
					level: checkOK,
					key:   "version",
					value: v.String(),
				})
			}
		}

		// Plugin check when wired. This check compares the BODY (InstalledShim + os.Stat of the
		// shim), never a target re-resolved here, because on Windows resolveOpenCodeInstall copies
		// the binary, which doctor must never do.
		if wired {
			pluginPath := opencode.PluginPath()
			body, readErr := os.ReadFile(pluginPath)
			if readErr != nil {
				checks = append(checks, captureCheck{
					level: checkFail,
					key:   "plugin",
					value: "cannot read",
					why:   fmt.Sprintf("plugin file expected at %s: %v", pluginPath, readErr),
					fix:   [2]string{"re-wire", "witness wire opencode"},
				})
			} else {
				shim, current := plugin.InstalledShim(string(body))
				if !current {
					checks = append(checks, captureCheck{
						level: checkFail,
						key:   "plugin",
						value: "out of date",
						why:   "the installed plugin body does not match the current witness version",
						fix:   [2]string{"re-wire", "witness wire opencode"},
					})
				} else {
					// Check shim file exists
					if _, err := os.Stat(shim); err != nil {
						checks = append(checks, captureCheck{
							level: checkFail,
							key:   "plugin",
							value: "shim missing",
							why:   fmt.Sprintf("the plugin points to %s, which does not exist", shim),
							fix:   [2]string{"re-wire", "witness wire opencode"},
						})
					} else {
						checks = append(checks, captureCheck{
							level: checkOK,
							key:   "plugin",
							value: "current",
						})
					}
				}
			}

			// MCP check when wired
			data, err := os.ReadFile(opencodeConfigPath())
			if err != nil {
				checks = append(checks, captureCheck{
					level: checkWarn,
					key:   "MCP entry",
					value: "config missing",
					why:   fmt.Sprintf("cannot read opencode.json at %s: %v", opencodeConfigPath(), err),
					fix:   [2]string{"re-wire", "witness wire opencode"},
				})
			} else {
				root, perr := parseOpenCodeConfig(data)
				if perr != nil {
					checks = append(checks, captureCheck{
						level: checkWarn,
						key:   "MCP entry",
						value: "config unparseable",
						why:   fmt.Sprintf("cannot parse opencode.json: %v", perr),
						fix:   [2]string{"re-wire", "witness wire opencode"},
					})
				} else {
					mcp, _ := root["mcp"].(map[string]any)
					if mcp["witness"] == nil {
						checks = append(checks, captureCheck{
							level: checkWarn,
							key:   "MCP entry",
							value: "missing",
							why:   "mcp.witness is not in opencode.json; the plugin may not be able to call record_observation",
							fix:   [2]string{"re-wire", "witness wire opencode"},
						})
					}
				}
			}
		}
	}

	// Both paths: database check
	if dbPathErr != nil {
		checks = append(checks, captureCheck{
			level: checkFail,
			key:   "database",
			value: "cannot resolve path",
			why:   fmt.Sprintf("cannot determine OpenCode database location: %v", dbPathErr),
		})
	} else if dbErr != nil {
		if errors.Is(dbErr, fs.ErrNotExist) {
			checks = append(checks, captureCheck{
				level: checkOK,
				key:   "database",
				value: "none yet",
			})
		} else {
			checks = append(checks, captureCheck{
				level: checkFail,
				key:   "database",
				value: "unreadable",
				why:   fmt.Sprintf("witness cannot read the OpenCode database at %s: %v", dbPath, dbErr),
			})
		}
	} else {
		checks = append(checks, captureCheck{
			level: checkOK,
			key:   "database",
			value: "readable",
		})
	}

	// Both paths: freshness check when database exists and has activity
	if dbErr == nil && !newest.IsZero() {
		stale := newest.Sub(seen) > opencode.StaleAfter
		if stale {
			seenStr := "never"
			if !seen.IsZero() {
				seenStr = shortTime(seen.Format(time.RFC3339))
			}
			checks = append(checks, captureCheck{
				level: checkWarn,
				key:   "freshness",
				value: fmt.Sprintf("OpenCode has activity from %s that witness has not imported (last import saw %s)", shortTime(newest.Format(time.RFC3339)), seenStr),
				why:   "the plugin may not be loading — OpenCode reports a plugin it rejects only in its own log",
				fix:   [2]string{"import now", "witness import --agent opencode"},
			})
		} else {
			checks = append(checks, captureCheck{
				level: checkOK,
				key:   "freshness",
				value: "current",
			})
		}
	}

	return checks
}
