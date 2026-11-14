package commands

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/IngTian/witness/internal/distill"
	"github.com/IngTian/witness/internal/platform/opencode"
	"github.com/IngTian/witness/internal/platform/opencode/plugin"
	"github.com/IngTian/witness/internal/store"

	_ "modernc.org/sqlite"
)

// openCodeHealthEnv sandboxes one health test: a fresh archive and OpenCode config dir, and a seeded
// v2 DB whose one session went idle at newestMs. It returns WITNESS_HOME, where a shim can go.
func openCodeHealthEnv(t *testing.T, newestMs int64) (home string) {
	t.Helper()
	home = t.TempDir()
	t.Setenv("WITNESS_HOME", home)
	t.Setenv("OPENCODE_CONFIG_DIR", t.TempDir())
	t.Setenv("WITNESS_OPENCODE_DB", seedOpenCodeV2DB(t, newestMs))
	return home
}

// currentOpenCodePlugin plants this build's plugin around a shim that exists, so the plugin row
// passes and only what a test changes can fail. It returns the shim.
func currentOpenCodePlugin(t *testing.T, home string) string {
	t.Helper()
	shim := filepath.Join(home, "witness.sh")
	if err := os.WriteFile(shim, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	plantOpenCodePlugin(t, plugin.Source(shim))
	return shim
}

// markOpenCodeSeen records the newest activity an import saw, as Import does; zero records none.
// It opens and closes the archive itself, so the doctor or status run that follows opens it alone.
func markOpenCodeSeen(t *testing.T, at time.Time) {
	t.Helper()
	if at.IsZero() {
		return
	}
	st, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.SetMetaString("opencode_seen_time_updated_ms", strconv.FormatInt(at.UnixMilli(), 10)); err != nil {
		t.Fatal(err)
	}
}

// openCodeChecks runs the health checks once: quick is status's set, otherwise doctor's.
func openCodeChecks(t *testing.T, quick bool) []captureCheck {
	t.Helper()
	st, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	return openCodeHealth(st, quick)
}

// healthRow is the row under key, failing the test when there is none.
func healthRow(t *testing.T, checks []captureCheck, key string) captureCheck {
	t.Helper()
	for _, c := range checks {
		if c.key == key {
			return c
		}
	}
	t.Fatalf("no %q row in %+v", key, checks)
	return captureCheck{}
}

// forbidOpenCodeVersionProbe fails the test if anything probes OpenCode's version: status runs on
// every call and must never spawn `opencode --version`.
func forbidOpenCodeVersionProbe(t *testing.T) {
	prev := openCodeVersion
	openCodeVersion = func(context.Context) (opencode.Version, error) {
		t.Fatal("OpenCode's version was probed")
		panic("unreachable")
	}
	t.Cleanup(func() { openCodeVersion = prev })
}

// doctorCanDistill makes doctor's other checks pass (prompts, an embedder, a mine model), so its
// exit code is the capture block's alone.
func doctorCanDistill(t *testing.T) {
	t.Helper()
	t.Setenv("WITNESS_PROMPTS", filepath.Join("..", "..", "prompts"))
	prev := newDoctorEmbedder
	newDoctorEmbedder = func() (distill.Embedder, error) { return fixedEmbedder{vec: []float32{1, 0, 0}}, nil }
	t.Cleanup(func() { newDoctorEmbedder = prev })
	st, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := configApplySet(st, "mine_model", "claude-haiku-4-5", ""); err != nil {
		t.Fatal(err)
	}
}

// assertJSONHasNoCaptureRows: the capture block is human output only; --json never changes.
func assertJSONHasNoCaptureRows(t *testing.T, out string) {
	t.Helper()
	var report map[string]any
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("JSON output not parseable: %v\n%s", err, out)
	}
	if _, ok := report["capture"]; ok {
		t.Errorf("JSON output has a 'capture' key, should be unchanged")
	}
}

func TestOpenCodeHealthIsSilentWhenOpenCodeIsNotInUse(t *testing.T) {
	t.Setenv("WITNESS_HOME", t.TempDir())
	t.Setenv("OPENCODE_CONFIG_DIR", t.TempDir())
	t.Setenv("WITNESS_OPENCODE_DB", filepath.Join(t.TempDir(), "missing.db"))
	for _, quick := range []bool{false, true} {
		if checks := openCodeChecks(t, quick); checks != nil {
			t.Errorf("quick=%v: got %+v, want nil (silent)", quick, checks)
		}
	}
}

func TestOpenCodeHealthVersionLevels(t *testing.T) {
	for _, tc := range []struct {
		name     string
		v        opencode.Version
		probeErr error
		level    checkLevel
		value    string
		whyHas   string
		fix      [2]string
	}{
		{"v1", opencode.Version{Major: 1, Minor: 19, Patch: 5}, nil, checkFail, "1.19.5", "", [2]string{"upgrade", "opencode upgrade --major"}},
		{"below 2.2.7", opencode.Version{Major: 2, Minor: 2, Patch: 6}, nil, checkFail, "2.2.6", "", [2]string{"upgrade", "opencode upgrade"}},
		{"newest tested", opencode.Version{Major: 2, Minor: 3, Patch: 0}, nil, checkOK, "2.3.0", "", [2]string{}},
		{"above the newest tested minor", opencode.Version{Major: 2, Minor: 4, Patch: 0}, nil, checkWarn, "2.4.0", "2.3", [2]string{}},
		{"probe error", opencode.Version{}, os.ErrNotExist, checkWarn, "cannot probe", "2.2.7", [2]string{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			openCodeHealthEnv(t, time.Now().UnixMilli())
			plantOpenCodePlugin(t, plugin.Source("/r/witness.sh"))
			stubOpenCodeVersion(t, tc.v, tc.probeErr)
			c := healthRow(t, openCodeChecks(t, false), "version")
			if c.level != tc.level || c.value != tc.value || !strings.Contains(c.why, tc.whyHas) || c.fix != tc.fix {
				t.Errorf("version row = %+v, want level %d, value %q, why naming %q, fix %q", c, tc.level, tc.value, tc.whyHas, tc.fix)
			}
		})
	}
}

// Every row the doctor path prints about the wiring itself, by the state that produces it.
func TestOpenCodeHealthPluginMCPAndDatabaseRows(t *testing.T) {
	const wiredConfig = `{"mcp":{"witness":{"command":"witness","args":["mcp"]}}}`
	current := plugin.Source
	for _, tc := range []struct {
		name   string
		body   func(shim string) string // shim is a real file
		config string                   // opencode.json; "" = none
		db     func(*testing.T) string  // nil: the seeded v2 DB
		key    string
		level  checkLevel
		value  string // "" = no row under key
		has    string // in why + fix
	}{
		// A valid shim line (an interpreted string, so the line parses), so the v1 BODY is what fails it
		{"v1 body", func(shim string) string {
			q, _ := json.Marshal(shim)
			return "globalThis.WITNESS_SHIM = " + string(q) + "\n\nconst plugin = async () => ({})\nexport default plugin\n"
		}, "", nil, "plugin", checkFail, "out of date", "witness wire opencode"},
		// An older witness build's v2 body still captures: a warning, never a failed doctor
		{"older v2 body", func(shim string) string { return current(shim) + "\n// a line this build does not have\n" },
			"", nil, "plugin", checkWarn, "an older witness build", "witness wire opencode"},
		{"v2 body with an unparseable shim line", func(shim string) string {
			_, rest, _ := strings.Cut(current(shim), "\n")
			return "globalThis.WITNESS_SHIM = not json\n" + rest
		}, "", nil, "plugin", checkFail, "out of date", "witness wire opencode"},
		{"current body", current, "", nil, "plugin", checkOK, "current", ""},
		{"missing shim", func(string) string { return current("/missing/witness.sh") }, "", nil, "plugin", checkFail, "shim missing", "/missing"},
		{"no MCP entry", current, "{}", nil, "MCP entry", checkWarn, "missing", ""},
		{"missing config", current, "", nil, "MCP entry", checkWarn, "config missing", ""},
		{"unparseable config", current, "{not json", nil, "MCP entry", checkWarn, "config unparseable", "witness wire opencode"},
		{"correctly wired config", current, wiredConfig, nil, "MCP entry", 0, "", ""},
		{"v1-only database", current, "", v1OnlyOpenCodeDB, "database", checkFail, "unreadable", "OpenCode v1"},
		// Wired before OpenCode's first run
		{"no database yet", current, "", func(t *testing.T) string { return filepath.Join(t.TempDir(), "opencode.db") }, "database", checkOK, "none yet", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			shim := currentOpenCodePlugin(t, openCodeHealthEnv(t, time.Now().UnixMilli()))
			plantOpenCodePlugin(t, tc.body(shim))
			if tc.db != nil {
				t.Setenv("WITNESS_OPENCODE_DB", tc.db(t))
			}
			if tc.config != "" {
				if err := os.WriteFile(filepath.Join(opencode.ConfigDir(), "opencode.json"), []byte(tc.config), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			checks := openCodeChecks(t, false)
			if tc.value == "" {
				for _, c := range checks {
					if c.key == tc.key {
						t.Errorf("found a %q row: %+v", tc.key, c)
					}
				}
				return
			}
			c := healthRow(t, checks, tc.key)
			if c.level != tc.level || c.value != tc.value || !strings.Contains(c.why+c.fix[0]+c.fix[1], tc.has) {
				t.Errorf("%s row = %+v, want level %d, value %q, naming %q", tc.key, c, tc.level, tc.value, tc.has)
			}
		})
	}
}

func TestOpenCodeHealthSkipsThePluginRowForNpmUsers(t *testing.T) {
	openCodeHealthEnv(t, time.Now().UnixMilli())
	markOpenCodeSeen(t, time.Now().Add(-time.Hour)) // imported, and no plugin file
	for _, c := range openCodeChecks(t, false) {
		if c.key == "plugin" || c.key == "MCP entry" {
			t.Errorf("found %q check when plugin is not present (npm user)", c.key)
		}
	}
}

func TestOpenCodeHealthWarnsWhenOpenCodeIsAheadOfTheImport(t *testing.T) {
	now := time.Now()
	for _, tc := range []struct {
		name         string
		newest, seen time.Time // seen zero: never imported
		level        checkLevel
	}{
		{"1h behind", now, now.Add(-time.Hour), checkWarn},
		{"caught up", now.Add(-time.Minute), now, checkOK},
		{"never imported", now, time.Time{}, checkWarn},
	} {
		t.Run(tc.name, func(t *testing.T) {
			currentOpenCodePlugin(t, openCodeHealthEnv(t, tc.newest.UnixMilli()))
			markOpenCodeSeen(t, tc.seen)
			c := healthRow(t, openCodeChecks(t, false), "freshness")
			if c.level != tc.level {
				t.Errorf("level = %d, want %d", c.level, tc.level)
			}
			if tc.level == checkWarn && c.fix != [2]string{"import now", "witness import --agent opencode"} {
				t.Errorf("fix = %q, want the import command", c.fix)
			}
		})
	}
}

func TestQuickHealthNeverProbesTheVersion(t *testing.T) {
	openCodeHealthEnv(t, time.Now().UnixMilli())
	plantOpenCodePlugin(t, plugin.Source("/r/witness.sh"))
	markOpenCodeSeen(t, time.Now().Add(-time.Hour))
	forbidOpenCodeVersionProbe(t)

	keys := map[string]bool{}
	for _, c := range openCodeChecks(t, true) {
		keys[c.key] = true
	}
	for _, k := range []string{"version", "plugin", "MCP entry"} {
		if keys[k] {
			t.Errorf("found %q check on quick path", k)
		}
	}
	for _, k := range []string{"database", "freshness"} {
		if !keys[k] {
			t.Errorf("quick path missing %q check", k)
		}
	}
}

func TestDoctorShowsTheCaptureBlockAndFailsOnACaptureFailure(t *testing.T) {
	shim := currentOpenCodePlugin(t, openCodeHealthEnv(t, time.Now().UnixMilli()))
	doctorCanDistill(t)
	for _, step := range []struct {
		name    string
		body    string
		wantErr bool
		want    []string // in the human output
	}{
		{"current plugin", plugin.Source(shim), false, []string{"Capture · opencode"}},
		// An older witness build's v2 body still captures: a warning, and doctor passes
		{"older v2 plugin", plugin.Source(shim) + "\n// an older build\n", false, []string{"plugin: an older witness build", "witness wire opencode"}},
		// A valid shim line and a v1 body
		{"v1 plugin", "globalThis.WITNESS_SHIM = \"/r\"\n\nconst plugin = async () => ({})\nexport default plugin\n", true, []string{"Capture · opencode", "plugin", "out of date"}},
	} {
		plantOpenCodePlugin(t, step.body)
		out, err := captureStdoutErr(t, func() error { return cmdDoctor(false) })
		if (err != nil) != step.wantErr {
			t.Errorf("%s: cmdDoctor(false) = %v, want an error: %v", step.name, err, step.wantErr)
		}
		for _, w := range step.want {
			if !strings.Contains(out, w) {
				t.Errorf("%s: human output missing %q; got:\n%s", step.name, w, out)
			}
		}
		out, err = captureStdoutErr(t, func() error { return cmdDoctor(true) })
		if (err != nil) != step.wantErr {
			t.Errorf("%s: cmdDoctor(true) = %v, want an error: %v", step.name, err, step.wantErr)
		}
		assertJSONHasNoCaptureRows(t, out)
	}
}

func TestStatusWarnsWhenOpenCodeCaptureIsStale(t *testing.T) {
	currentOpenCodePlugin(t, openCodeHealthEnv(t, time.Now().UnixMilli()))
	forbidOpenCodeVersionProbe(t)

	markOpenCodeSeen(t, time.Now().Add(-time.Hour))
	stale := captureStdout(t, func() { _ = cmdDistillStatus(false) })
	// Under the editor's name: "freshness: …" alone does not say whose capture is behind
	for _, want := range []string{"opencode freshness: ", "witness import --agent opencode", "plugin may not be loading"} {
		if !strings.Contains(stale, want) {
			t.Errorf("stale status output missing %q; got:\n%s", want, stale)
		}
	}

	markOpenCodeSeen(t, time.Now())
	healthy := captureStdout(t, func() { _ = cmdDistillStatus(false) })
	// The capture warning's own words, not any mention of "opencode"
	if strings.Contains(healthy, "freshness:") || strings.Contains(healthy, "OpenCode has activity") {
		t.Errorf("healthy output has OpenCode capture warning when it should not; got:\n%s", healthy)
	}

	assertJSONHasNoCaptureRows(t, captureStdout(t, func() { _ = cmdDistillStatus(true) }))
}

// An OpenCode the user never wired, or just unwired, whose DB has sessions: doctor says so in one
// neutral row with the wire step. No freshness verdict, no import command and no version verdict,
// so not even an out-of-range OpenCode can fail doctor for someone who never opted into capturing it.
func TestDoctorOnlyNotesAnUnwiredOpenCodeWithSessions(t *testing.T) {
	for _, unwired := range []bool{false, true} {
		name := "never wired"
		if unwired {
			name = "just unwired"
		}
		t.Run(name, func(t *testing.T) {
			home := openCodeHealthEnv(t, time.Now().UnixMilli())
			t.Setenv("OPENCODE_CONFIG", "")
			doctorCanDistill(t)
			stubOpenCodeVersion(t, opencode.Version{Major: 2, Minor: 2, Patch: 6}, nil)
			if unwired {
				// Wired, imported once, then unwired
				plantOpenCodePlugin(t, plugin.Source(filepath.Join(home, "witness.sh")))
				markOpenCodeSeen(t, time.Now().Add(-time.Hour))
				captureStdout(t, func() { _ = cmdUninstall([]string{"opencode"}) })
			}

			full, quick := openCodeChecks(t, false), openCodeChecks(t, true)
			if len(full) != 1 || full[0].level != checkInfo || full[0].fix[1] != "witness wire opencode" {
				t.Errorf("doctor checks = %+v, want one checkInfo row whose fix is `witness wire opencode`", full)
			}
			if quick != nil {
				t.Errorf("status checks = %+v, want none", quick)
			}

			out, err := captureStdoutErr(t, func() error { return cmdDoctor(false) })
			if err != nil {
				t.Errorf("doctor returned %v, want nil (an unwired OpenCode is not a capture failure)", err)
			}
			if !strings.Contains(out, "sessions here, not wired") || !strings.Contains(out, "witness wire opencode") {
				t.Errorf("doctor output missing the not-wired row and its wire step; got:\n%s", out)
			}
			for _, bad := range []string{"freshness", "witness import --agent opencode", "2.2.6"} {
				if strings.Contains(out, bad) {
					t.Errorf("doctor output has %q for an unwired OpenCode; got:\n%s", bad, out)
				}
			}
		})
	}
}

// Under the npm launcher wire is refused, so an npm plugin that never loaded is pointed at the
// plugins list instead.
func TestTheNotWiredRowNamesTheNpmPluginUnderNpm(t *testing.T) {
	openCodeHealthEnv(t, time.Now().UnixMilli())
	t.Setenv("WITNESS_NPM_PACKAGE", "1")
	if checks := openCodeChecks(t, false); len(checks) != 1 || checks[0].fix[1] != npmWireStep {
		t.Fatalf("checks = %+v, want one row whose fix is %q", checks, npmWireStep)
	}
}

// status runs on every call: a machine that has OpenCode but never wired it must not have its DB
// opened (a schema probe plus a scan of session_message) by `witness status`.
func TestStatusNeverOpensTheDatabaseOfAnUnwiredOpenCode(t *testing.T) {
	openCodeHealthEnv(t, time.Now().UnixMilli())
	opens := 0
	prev := openCodeNewestActivity
	openCodeNewestActivity = func(ctx context.Context, path string) (time.Time, error) {
		opens++
		return prev(ctx, path)
	}
	t.Cleanup(func() { openCodeNewestActivity = prev })

	if checks := openCodeChecks(t, true); checks != nil {
		t.Errorf("quick checks = %+v, want none", checks)
	}
	captureStdout(t, func() { _ = cmdDistillStatus(false) })
	if opens != 0 {
		t.Fatalf("status opened the OpenCode DB %d times for an OpenCode that was never wired, want 0", opens)
	}
	// The seam is the one health reads through: doctor still opens the DB to find the sessions
	if openCodeChecks(t, false) == nil || opens == 0 {
		t.Fatalf("doctor found no sessions (opens = %d); the counting seam is not the one health uses", opens)
	}
}
