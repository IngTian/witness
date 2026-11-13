package commands

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"strings"
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

// openCodeNewestActivity is opencode.NewestActivity, a var so a test can prove `witness status`
// never opens the OpenCode DB of a machine that never wired OpenCode.
var openCodeNewestActivity = opencode.NewestActivity

// checkOpenCodeForWire probes OpenCode and its database before wiring. It returns a slice of
// result rows to print (one per check), or an error that refuses wiring. An OpenCode whose version
// cannot be read is a warning only (wiring continues so capture works once it is installed).
// Unsupported versions and unreadable databases refuse before any write.
func checkOpenCodeForWire(ctx context.Context) (rows []string, err error) {
	v, probeErr := openCodeVersion(ctx)
	switch {
	case errors.Is(probeErr, exec.ErrNotFound):
		rows = append(rows, resultRow(warnGlyph(), "OpenCode", "not found", fmt.Sprintf("capture needs OpenCode %s+", opencode.MinVersion)))
	case probeErr != nil:
		// Not "not found": the Windows desktop app has no CLI on PATH, and a probe that times out
		// or prints no version is an OpenCode that is there
		rows = append(rows, resultRow(warnGlyph(), "OpenCode", "version unknown", "could not check OpenCode's version: "+probeErr.Error()))
	default:
		if err := opencode.CheckVersion(v); err != nil {
			return nil, err
		}
		if opencode.AboveTested(v) {
			rows = append(rows, resultRow(warnGlyph(), "OpenCode", v.String(), fmt.Sprintf("newer than witness has been tested with (%d.%d.x); doctor will say if capture breaks", opencode.NewestTestedMinor.Major, opencode.NewestTestedMinor.Minor)))
		} else {
			rows = append(rows, resultRow(okGlyph(), "OpenCode", v.String(), ""))
		}
	}

	// Whatever the probe said: a DB that is v1-only or mid-migration refuses before anything is
	// written, and a missing CLI says nothing about the DB
	dbPath, err := opencode.DefaultDBPath()
	if err != nil {
		return nil, fmt.Errorf("resolve opencode db path: %w", err)
	}
	_, err = openCodeNewestActivity(ctx, dbPath)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}

	return rows, nil
}

// stopOpenCodeCapture is unwire's last part. Unwire is the consent switch, and removing the files
// is not enough: an earlier import (the plugin's, or one manual `witness import --agent opencode`)
// keeps every later drain importing OpenCode, and status and doctor checking it, until
// ForgetCapture clears it.
func stopOpenCodeCapture() (stopped bool, warning string, err error) {
	st, err := store.Open()
	if err != nil {
		fmt.Println(resultRow(warnGlyph(), "capture", "not stopped", "could not open the archive: "+err.Error()))
		return false, "drains may still import OpenCode", nil
	}
	defer st.Close()
	if opencode.LastImported(st).IsZero() {
		return false, "", nil
	}
	if err := opencode.ForgetCapture(st); err != nil {
		return false, "", fmt.Errorf("stop OpenCode capture: %w", err)
	}
	fmt.Println(resultRow(okGlyph(), "capture", "stopped", "drains no longer import OpenCode"))
	return true, "", nil
}

// openCodeHealth checks OpenCode capture status for doctor and status. Returns nil (silent)
// when OpenCode is not in use. quick skips version and plugin checks (for status).
func openCodeHealth(st *store.Store, quick bool) []captureCheck {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// In use: a plugin file, or an import since the last unwire (an npm user has no plugin file)
	wired := opencode.PluginInstalled()
	seen := opencode.LastImported(st)
	inUse := wired || !seen.IsZero()
	// Status runs often: it must not open (and scan) the DB of anyone who merely has OpenCode
	if quick && !inUse {
		return nil
	}
	dbPath, dbPathErr := opencode.DefaultDBPath()
	var newest time.Time
	var dbErr error
	if dbPathErr == nil {
		newest, dbErr = openCodeNewestActivity(ctx, dbPath)
	}
	if !inUse {
		// Doctor still speaks when the DB has sessions (spec §7.8), but OpenCode was never wired
		// here or was unwired: there is no plugin to blame and no capture to fail, so one neutral
		// row, never a freshness verdict, an import command or a failed version
		if dbErr != nil || newest.IsZero() {
			return nil
		}
		wire := "witness wire opencode"
		if os.Getenv("WITNESS_NPM_PACKAGE") == "1" {
			wire = npmWireStep // the npm launcher refuses wire
		}
		return []captureCheck{{level: checkInfo, key: "opencode", value: "sessions here, not wired", fix: [2]string{"capture them", wire}}}
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
				src := string(body)
				shim, current := plugin.InstalledShim(src)
				// Only a v1 or unparseable file fails: an older witness build's v2 body still
				// captures, and failing on any byte difference would fail every wired user's
				// doctor after each release that edits witness.js, until they re-wire. v2 loads
				// only `export default { id, setup }`, which no v1 body has.
				v2 := shim != "" && strings.Contains(src, "export default {") && strings.Contains(src, `id: "witness"`)
				_, shimErr := os.Stat(shim)
				switch {
				case !current && !v2:
					checks = append(checks, captureCheck{
						level: checkFail,
						key:   "plugin",
						value: "out of date",
						why:   "the installed plugin is a v1 file or was edited by hand; OpenCode v2 may not load it",
						fix:   [2]string{"re-wire", "witness wire opencode"},
					})
				case shimErr != nil:
					checks = append(checks, captureCheck{
						level: checkFail,
						key:   "plugin",
						value: "shim missing",
						why:   fmt.Sprintf("the plugin points to %s, which does not exist", shim),
						fix:   [2]string{"re-wire", "witness wire opencode"},
					})
				case !current:
					checks = append(checks, captureCheck{
						level: checkWarn,
						key:   "plugin",
						value: "an older witness build",
						why:   "it still captures; re-wire to update it to this build's copy",
						fix:   [2]string{"re-wire", "witness wire opencode"},
					})
				default:
					checks = append(checks, captureCheck{
						level: checkOK,
						key:   "plugin",
						value: "current",
					})
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
							why:   "mcp.witness is not in opencode.json; OpenCode's agent will not see witness's MCP tools",
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
