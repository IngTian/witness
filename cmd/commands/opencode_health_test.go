package commands

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/IngTian/witness/internal/distill"
	"github.com/IngTian/witness/internal/platform/opencode"
	"github.com/IngTian/witness/internal/platform/opencode/plugin"
	"github.com/IngTian/witness/internal/store"

	_ "modernc.org/sqlite"
)

func TestOpenCodeHealthIsSilentWhenOpenCodeIsNotInUse(t *testing.T) {
	t.Setenv("WITNESS_HOME", t.TempDir())
	t.Setenv("OPENCODE_CONFIG_DIR", t.TempDir())
	t.Setenv("WITNESS_OPENCODE_DB", filepath.Join(t.TempDir(), "missing.db"))

	st, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	for _, quick := range []bool{false, true} {
		checks := openCodeHealth(st, quick)
		if checks != nil {
			t.Errorf("quick=%v: got %d checks, want nil (silent)", quick, len(checks))
		}
	}
}

func TestOpenCodeHealthVersionLevels(t *testing.T) {
	cases := []struct {
		version     opencode.Version
		wantLevel   checkLevel
		wantContain string
	}{
		{opencode.Version{Major: 1, Minor: 19, Patch: 5}, checkFail, "opencode upgrade --major"},
		{opencode.Version{Major: 2, Minor: 2, Patch: 6}, checkFail, "opencode upgrade"},
		{opencode.Version{Major: 2, Minor: 3, Patch: 0}, checkOK, "2.3.0"},
		{opencode.Version{Major: 2, Minor: 4, Patch: 0}, checkWarn, "2.4.0"},
		{opencode.Version{Major: 2, Minor: 4, Patch: 0}, checkWarn, "2.3"},
	}

	for _, tc := range cases {
		name := tc.version.String()
		t.Run(name, func(t *testing.T) {
			home := t.TempDir()
			configDir := t.TempDir()
			t.Setenv("WITNESS_HOME", home)
			t.Setenv("OPENCODE_CONFIG_DIR", configDir)
			dbPath := seedOpenCodeV2DB(t, time.Now().UnixMilli())
			t.Setenv("WITNESS_OPENCODE_DB", dbPath)

			st, err := store.Open()
			if err != nil {
				t.Fatal(err)
			}
			defer st.Close()

			// Plant a plugin so the gate passes
			if err := os.MkdirAll(filepath.Join(configDir, "plugins"), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(opencode.PluginPath(), []byte(plugin.Source("/r/witness.sh")), 0o644); err != nil {
				t.Fatal(err)
			}

			// Mark as seen to trigger freshness check later
			st.SetMetaString("opencode_seen_time_updated_ms", fmt.Sprintf("%d", time.Now().Add(-1*time.Hour).UnixMilli()))

			oldVersion := openCodeVersion
			openCodeVersion = func(context.Context) (opencode.Version, error) { return tc.version, nil }
			t.Cleanup(func() { openCodeVersion = oldVersion })

			checks := openCodeHealth(st, false)
			var found *captureCheck
			for i := range checks {
				if checks[i].key == "version" {
					found = &checks[i]
					break
				}
			}
			if found == nil {
				t.Fatalf("no version check found")
			}
			text := found.key + found.value + found.why + found.fix[0] + found.fix[1]
			if found.level != tc.wantLevel {
				t.Errorf("level = %d, want %d", found.level, tc.wantLevel)
			}
			if tc.wantContain != "" && !strings.Contains(text, tc.wantContain) {
				t.Errorf("text %q does not contain %q", text, tc.wantContain)
			}
		})
	}

	// probe error → checkWarn naming 2.2.7
	t.Run("probe error", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("WITNESS_HOME", home)
		t.Setenv("OPENCODE_CONFIG_DIR", t.TempDir())
		dbPath := seedOpenCodeV2DB(t, time.Now().UnixMilli())
		t.Setenv("WITNESS_OPENCODE_DB", dbPath)

		st, err := store.Open()
		if err != nil {
			t.Fatal(err)
		}
		defer st.Close()

		if err := os.MkdirAll(filepath.Dir(opencode.PluginPath()), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(opencode.PluginPath(), []byte(plugin.Source("/r/witness.sh")), 0o644); err != nil {
			t.Fatal(err)
		}
		st.SetMetaString("opencode_seen_time_updated_ms", fmt.Sprintf("%d", time.Now().Add(-1*time.Hour).UnixMilli()))

		oldVersion := openCodeVersion
		openCodeVersion = func(context.Context) (opencode.Version, error) { return opencode.Version{}, os.ErrNotExist }
		t.Cleanup(func() { openCodeVersion = oldVersion })

		checks := openCodeHealth(st, false)
		var found *captureCheck
		for i := range checks {
			if checks[i].key == "version" {
				found = &checks[i]
				break
			}
		}
		if found == nil {
			t.Fatal("no version check found")
		}
		if found.level != checkWarn {
			t.Errorf("level = %d, want checkWarn", found.level)
		}
		if !strings.Contains(found.value+found.why, "2.2.7") {
			t.Errorf("text does not name 2.2.7")
		}
	})
}

func TestOpenCodeHealthFlagsAnOutOfDatePlugin(t *testing.T) {
	cases := []struct {
		name        string
		pluginBody  func(shim string) string // shim is a real file
		wantLevel   checkLevel
		wantKey     string
		wantContain string
	}{
		{
			// A valid shim line, so the v1 BODY is what fails it
			name: "v1 body",
			pluginBody: func(shim string) string {
				q, _ := json.Marshal(shim)
				return "globalThis.WITNESS_SHIM = " + string(q) + "\n\nconst plugin = async () => ({})\nexport default plugin\n"
			},
			wantLevel:   checkFail,
			wantKey:     "plugin",
			wantContain: "witness wire opencode",
		},
		{
			// An older witness build's v2 body still captures: a warning, never a failed doctor
			name:        "older v2 body",
			pluginBody:  func(shim string) string { return plugin.Source(shim) + "\n// a line this build does not have\n" },
			wantLevel:   checkWarn,
			wantKey:     "plugin",
			wantContain: "witness wire opencode",
		},
		{
			name: "v2 body with an unparseable shim line",
			pluginBody: func(shim string) string {
				_, rest, _ := strings.Cut(plugin.Source(shim), "\n")
				return "globalThis.WITNESS_SHIM = not json\n" + rest
			},
			wantLevel:   checkFail,
			wantKey:     "plugin",
			wantContain: "witness wire opencode",
		},
		{
			name:       "current body",
			pluginBody: plugin.Source,
			wantLevel:  checkOK,
			wantKey:    "plugin",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("WITNESS_HOME", home)
			t.Setenv("OPENCODE_CONFIG_DIR", t.TempDir())
			dbPath := seedOpenCodeV2DB(t, time.Now().UnixMilli())
			t.Setenv("WITNESS_OPENCODE_DB", dbPath)

			st, err := store.Open()
			if err != nil {
				t.Fatal(err)
			}
			defer st.Close()

			if err := os.MkdirAll(filepath.Dir(opencode.PluginPath()), 0o700); err != nil {
				t.Fatal(err)
			}
			shimPath := filepath.Join(home, "witness.sh")
			if err := os.WriteFile(shimPath, []byte("#!/bin/sh\n"), 0o755); err != nil {
				t.Fatal(err)
			}
			body := tc.pluginBody(shimPath)
			if err := os.WriteFile(opencode.PluginPath(), []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}

			// Mark as freshly imported so freshness check passes
			st.SetMetaString("opencode_seen_time_updated_ms", fmt.Sprintf("%d", time.Now().UnixMilli()))

			checks := openCodeHealth(st, false)
			var found *captureCheck
			for i := range checks {
				if checks[i].key == tc.wantKey {
					found = &checks[i]
					break
				}
			}
			if found == nil {
				t.Fatalf("no check with key %q found", tc.wantKey)
			}
			if found.level != tc.wantLevel {
				t.Errorf("level = %d, want %d", found.level, tc.wantLevel)
			}
			if tc.wantContain != "" {
				text := found.value + found.why + found.fix[0] + found.fix[1]
				if !strings.Contains(text, tc.wantContain) {
					t.Errorf("text %q does not contain %q", text, tc.wantContain)
				}
			}
		})
	}

	// missing shim file
	t.Run("missing shim", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("WITNESS_HOME", home)
		t.Setenv("OPENCODE_CONFIG_DIR", t.TempDir())
		dbPath := seedOpenCodeV2DB(t, time.Now().UnixMilli())
		t.Setenv("WITNESS_OPENCODE_DB", dbPath)

		st, err := store.Open()
		if err != nil {
			t.Fatal(err)
		}
		defer st.Close()

		if err := os.MkdirAll(filepath.Dir(opencode.PluginPath()), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(opencode.PluginPath(), []byte(plugin.Source("/missing/witness.sh")), 0o644); err != nil {
			t.Fatal(err)
		}

		checks := openCodeHealth(st, false)
		var found *captureCheck
		for i := range checks {
			if checks[i].key == "plugin" {
				found = &checks[i]
				break
			}
		}
		if found == nil {
			t.Fatal("no plugin check found")
		}
		if found.level != checkFail {
			t.Errorf("level = %d, want checkFail", found.level)
		}
		if !strings.Contains(found.value+found.why, "/missing") {
			t.Errorf("text does not name the missing path")
		}
	})

	// no MCP entry
	t.Run("no MCP entry", func(t *testing.T) {
		home := t.TempDir()
		configDir := t.TempDir()
		t.Setenv("WITNESS_HOME", home)
		t.Setenv("OPENCODE_CONFIG_DIR", configDir)
		dbPath := seedOpenCodeV2DB(t, time.Now().UnixMilli())
		t.Setenv("WITNESS_OPENCODE_DB", dbPath)

		st, err := store.Open()
		if err != nil {
			t.Fatal(err)
		}
		defer st.Close()

		if err := os.MkdirAll(filepath.Dir(opencode.PluginPath()), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(opencode.PluginPath(), []byte(plugin.Source("/r/witness.sh")), 0o644); err != nil {
			t.Fatal(err)
		}

		// empty opencode.json
		if err := os.WriteFile(filepath.Join(configDir, "opencode.json"), []byte("{}"), 0o644); err != nil {
			t.Fatal(err)
		}

		checks := openCodeHealth(st, false)
		var found *captureCheck
		for i := range checks {
			if checks[i].key == "MCP entry" {
				found = &checks[i]
				break
			}
		}
		if found == nil {
			t.Fatal("no MCP entry check found")
		}
		if found.level != checkWarn {
			t.Errorf("level = %d, want checkWarn", found.level)
		}
	})

	// correctly wired config → no MCP warning
	t.Run("correctly wired config", func(t *testing.T) {
		home := t.TempDir()
		configDir := t.TempDir()
		t.Setenv("WITNESS_HOME", home)
		t.Setenv("OPENCODE_CONFIG_DIR", configDir)
		dbPath := seedOpenCodeV2DB(t, time.Now().UnixMilli())
		t.Setenv("WITNESS_OPENCODE_DB", dbPath)

		st, err := store.Open()
		if err != nil {
			t.Fatal(err)
		}
		defer st.Close()

		if err := os.MkdirAll(filepath.Dir(opencode.PluginPath()), 0o700); err != nil {
			t.Fatal(err)
		}
		shimPath := filepath.Join(home, "witness.sh")
		if err := os.WriteFile(shimPath, []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(opencode.PluginPath(), []byte(plugin.Source(shimPath)), 0o644); err != nil {
			t.Fatal(err)
		}

		// Write a correctly wired opencode.json with mcp.witness
		wiredConfig := `{"mcp":{"witness":{"command":"witness","args":["mcp"]}}}`
		if err := os.WriteFile(filepath.Join(configDir, "opencode.json"), []byte(wiredConfig), 0o644); err != nil {
			t.Fatal(err)
		}

		checks := openCodeHealth(st, false)
		for _, c := range checks {
			if c.key == "MCP entry" {
				t.Errorf("found 'MCP entry' check with correctly wired config: %+v", c)
			}
		}
	})

	// missing config → checkWarn
	t.Run("missing config", func(t *testing.T) {
		home := t.TempDir()
		configDir := t.TempDir()
		t.Setenv("WITNESS_HOME", home)
		t.Setenv("OPENCODE_CONFIG_DIR", configDir)
		dbPath := seedOpenCodeV2DB(t, time.Now().UnixMilli())
		t.Setenv("WITNESS_OPENCODE_DB", dbPath)

		st, err := store.Open()
		if err != nil {
			t.Fatal(err)
		}
		defer st.Close()

		if err := os.MkdirAll(filepath.Dir(opencode.PluginPath()), 0o700); err != nil {
			t.Fatal(err)
		}
		shimPath := filepath.Join(home, "witness.sh")
		if err := os.WriteFile(shimPath, []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(opencode.PluginPath(), []byte(plugin.Source(shimPath)), 0o644); err != nil {
			t.Fatal(err)
		}

		// No opencode.json file
		checks := openCodeHealth(st, false)
		var found *captureCheck
		for i := range checks {
			if checks[i].key == "MCP entry" {
				found = &checks[i]
				break
			}
		}
		if found == nil {
			t.Fatal("no MCP entry check found for missing config")
		}
		if found.level != checkWarn {
			t.Errorf("level = %d, want checkWarn", found.level)
		}
		if !strings.Contains(found.value, "missing") {
			t.Errorf("value should mention 'missing'; got %q", found.value)
		}
	})
}

func TestOpenCodeHealthSkipsThePluginRowForNpmUsers(t *testing.T) {
	home := t.TempDir()
	t.Setenv("WITNESS_HOME", home)
	t.Setenv("OPENCODE_CONFIG_DIR", t.TempDir())
	dbPath := seedOpenCodeV2DB(t, time.Now().UnixMilli())
	t.Setenv("WITNESS_OPENCODE_DB", dbPath)

	st, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	// Mark as seen but no plugin file
	st.SetMetaString("opencode_seen_time_updated_ms", fmt.Sprintf("%d", time.Now().Add(-1*time.Hour).UnixMilli()))

	checks := openCodeHealth(st, false)
	for _, c := range checks {
		if c.key == "plugin" || c.key == "MCP entry" {
			t.Errorf("found %q check when plugin is not present (npm user)", c.key)
		}
	}
}

func TestOpenCodeHealthFlagsADatabaseItCannotRead(t *testing.T) {
	home := t.TempDir()
	t.Setenv("WITNESS_HOME", home)
	t.Setenv("OPENCODE_CONFIG_DIR", t.TempDir())

	// v1-only DB: CREATE TABLE session (id TEXT PRIMARY KEY) with no session_v2
	v1db := filepath.Join(t.TempDir(), "opencode.db")
	db, err := sql.Open("sqlite", v1db)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE session (id TEXT PRIMARY KEY)`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	t.Setenv("WITNESS_OPENCODE_DB", v1db)

	st, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	if err := os.MkdirAll(filepath.Dir(opencode.PluginPath()), 0o700); err != nil {
		t.Fatal(err)
	}
	shimPath := filepath.Join(home, "witness.sh")
	if err := os.WriteFile(shimPath, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(opencode.PluginPath(), []byte(plugin.Source(shimPath)), 0o644); err != nil {
		t.Fatal(err)
	}

	checks := openCodeHealth(st, false)
	var found *captureCheck
	for i := range checks {
		if checks[i].key == "database" {
			found = &checks[i]
			break
		}
	}
	if found == nil {
		t.Fatal("no database check found")
	}
	if found.level != checkFail {
		t.Errorf("level = %d, want checkFail", found.level)
	}
	// Assert ErrOpenCodeV1's text is in the check
	if !strings.Contains(found.why, "OpenCode v1") && !strings.Contains(found.value, "OpenCode v1") {
		t.Errorf("database check missing ErrOpenCodeV1 text; got value=%q why=%q", found.value, found.why)
	}
}

func TestOpenCodeHealthWarnsWhenOpenCodeIsAheadOfTheImport(t *testing.T) {
	now := time.Now()
	cases := []struct {
		name         string
		newestMs     int64
		lastImportMs string
		wantLevel    checkLevel
		wantKey      string
		wantContain  string
	}{
		{
			name:         "1h behind",
			newestMs:     now.UnixMilli(),
			lastImportMs: fmt.Sprintf("%d", now.Add(-1*time.Hour).UnixMilli()),
			wantLevel:    checkWarn,
			wantKey:      "freshness",
			wantContain:  "witness import --agent opencode",
		},
		{
			name:         "caught up",
			newestMs:     now.Add(-1 * time.Minute).UnixMilli(),
			lastImportMs: fmt.Sprintf("%d", now.UnixMilli()),
			wantLevel:    checkOK,
			wantKey:      "freshness",
		},
		{
			name:         "never imported",
			newestMs:     now.UnixMilli(),
			lastImportMs: "",
			wantLevel:    checkWarn,
			wantKey:      "freshness",
			wantContain:  "witness import --agent opencode",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("WITNESS_HOME", home)
			t.Setenv("OPENCODE_CONFIG_DIR", t.TempDir())
			dbPath := seedOpenCodeV2DB(t, tc.newestMs)
			t.Setenv("WITNESS_OPENCODE_DB", dbPath)

			st, err := store.Open()
			if err != nil {
				t.Fatal(err)
			}
			defer st.Close()

			if err := os.MkdirAll(filepath.Dir(opencode.PluginPath()), 0o700); err != nil {
				t.Fatal(err)
			}
			// Create a real shim file so plugin check passes
			shimPath := filepath.Join(home, "witness.sh")
			if err := os.WriteFile(shimPath, []byte("#!/bin/sh\n"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(opencode.PluginPath(), []byte(plugin.Source(shimPath)), 0o644); err != nil {
				t.Fatal(err)
			}

			if tc.lastImportMs != "" {
				st.SetMetaString("opencode_seen_time_updated_ms", tc.lastImportMs)
			}

			checks := openCodeHealth(st, false)
			var found *captureCheck
			for i := range checks {
				if checks[i].key == tc.wantKey {
					found = &checks[i]
					break
				}
			}
			if found == nil {
				t.Fatalf("no check with key %q found", tc.wantKey)
			}
			if found.level != tc.wantLevel {
				t.Errorf("level = %d, want %d", found.level, tc.wantLevel)
			}
			if tc.wantContain != "" {
				text := found.value + found.why + found.fix[0] + found.fix[1]
				if !strings.Contains(text, tc.wantContain) {
					t.Errorf("text %q does not contain %q", text, tc.wantContain)
				}
			}
		})
	}
}

func TestQuickHealthNeverProbesTheVersion(t *testing.T) {
	home := t.TempDir()
	t.Setenv("WITNESS_HOME", home)
	t.Setenv("OPENCODE_CONFIG_DIR", t.TempDir())
	dbPath := seedOpenCodeV2DB(t, time.Now().UnixMilli())
	t.Setenv("WITNESS_OPENCODE_DB", dbPath)

	st, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	if err := os.MkdirAll(filepath.Dir(opencode.PluginPath()), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(opencode.PluginPath(), []byte(plugin.Source("/r/witness.sh")), 0o644); err != nil {
		t.Fatal(err)
	}
	st.SetMetaString("opencode_seen_time_updated_ms", fmt.Sprintf("%d", time.Now().Add(-1*time.Hour).UnixMilli()))

	oldVersion := openCodeVersion
	openCodeVersion = func(context.Context) (opencode.Version, error) {
		t.Fatal("version probed on quick path")
		panic("unreachable")
	}
	t.Cleanup(func() { openCodeVersion = oldVersion })

	checks := openCodeHealth(st, true)
	for _, c := range checks {
		if c.key == "version" || c.key == "plugin" || c.key == "MCP entry" {
			t.Errorf("found %q check on quick path", c.key)
		}
	}
	// Should have database and freshness only
	found := make(map[string]bool)
	for _, c := range checks {
		found[c.key] = true
	}
	for _, want := range []string{"database", "freshness"} {
		if !found[want] {
			t.Errorf("quick path missing %q check", want)
		}
	}
}

func TestDoctorShowsTheCaptureBlockAndFailsOnACaptureFailure(t *testing.T) {
	home := t.TempDir()
	configDir := t.TempDir()
	t.Setenv("WITNESS_HOME", home)
	t.Setenv("OPENCODE_CONFIG_DIR", configDir)
	t.Setenv("WITNESS_PROMPTS", filepath.Join("..", "..", "prompts"))
	dbPath := seedOpenCodeV2DB(t, time.Now().UnixMilli())
	t.Setenv("WITNESS_OPENCODE_DB", dbPath)

	// Use fixedEmbedder so the test doesn't fail from embedder unavailable
	prev := newDoctorEmbedder
	newDoctorEmbedder = func() (distill.Embedder, error) { return fixedEmbedder{vec: []float32{1, 0, 0}}, nil }
	t.Cleanup(func() { newDoctorEmbedder = prev })

	st, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	if err := configApplySet(st, "mine_model", "claude-haiku-4-5", ""); err != nil {
		t.Fatal(err)
	}
	st.Close()

	if err := os.MkdirAll(filepath.Dir(opencode.PluginPath()), 0o700); err != nil {
		t.Fatal(err)
	}

	// Test 1: current plugin → err == nil
	shimPath := filepath.Join(home, "witness.sh")
	if err := os.WriteFile(shimPath, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(opencode.PluginPath(), []byte(plugin.Source(shimPath)), 0o644); err != nil {
		t.Fatal(err)
	}

	humanOut, humanErr := captureStdoutErr(t, func() error { return cmdDoctor(false) })
	if humanErr != nil {
		t.Errorf("cmdDoctor(false) with current plugin returned error: %v", humanErr)
	}
	if !strings.Contains(humanOut, "Capture · opencode") {
		t.Errorf("human output missing 'Capture · opencode' block; got:\n%s", humanOut)
	}

	jsonOut, jsonErr := captureStdoutErr(t, func() error { return cmdDoctor(true) })
	if jsonErr != nil {
		t.Errorf("cmdDoctor(true) with current plugin returned error: %v", jsonErr)
	}
	var report map[string]any
	if err := json.Unmarshal([]byte(jsonOut), &report); err != nil {
		t.Fatalf("JSON output not parseable: %v", err)
	}
	// Assert no capture keys in JSON
	if _, ok := report["capture"]; ok {
		t.Errorf("JSON output has 'capture' key, should be unchanged")
	}

	// Test 2: an older witness build's v2 body still captures → a warning, err == nil
	if err := os.WriteFile(opencode.PluginPath(), []byte(plugin.Source(shimPath)+"\n// an older build\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	humanOut, humanErr = captureStdoutErr(t, func() error { return cmdDoctor(false) })
	if humanErr != nil {
		t.Errorf("cmdDoctor(false) with an older v2 plugin returned %v, want nil (it still captures)", humanErr)
	}
	if !strings.Contains(humanOut, "plugin: an older witness build") || !strings.Contains(humanOut, "witness wire opencode") {
		t.Errorf("human output missing the older-build warning and its fix; got:\n%s", humanOut)
	}

	// Test 3: a v1 plugin (valid shim line, v1 body) → err != nil
	if err := os.WriteFile(opencode.PluginPath(), []byte("globalThis.WITNESS_SHIM = \"/r\"\n\nconst plugin = async () => ({})\nexport default plugin\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	humanOut, humanErr = captureStdoutErr(t, func() error { return cmdDoctor(false) })
	if humanErr == nil {
		t.Error("cmdDoctor(false) with stale plugin returned nil, want non-nil")
	}
	if !strings.Contains(humanOut, "Capture · opencode") {
		t.Errorf("human output missing 'Capture · opencode' block; got:\n%s", humanOut)
	}
	if !strings.Contains(humanOut, "plugin") || !strings.Contains(humanOut, "out of date") {
		t.Errorf("human output missing plugin failure headline; got:\n%s", humanOut)
	}

	jsonOut, jsonErr = captureStdoutErr(t, func() error { return cmdDoctor(true) })
	if jsonErr == nil {
		t.Error("cmdDoctor(true) with stale plugin returned nil, want non-nil")
	}
	if err := json.Unmarshal([]byte(jsonOut), &report); err != nil {
		t.Fatalf("JSON output not parseable: %v", err)
	}
	if _, ok := report["capture"]; ok {
		t.Errorf("JSON output has 'capture' key, should be unchanged")
	}
}

func TestStatusWarnsWhenOpenCodeCaptureIsStale(t *testing.T) {
	home := t.TempDir()
	t.Setenv("WITNESS_HOME", home)
	t.Setenv("OPENCODE_CONFIG_DIR", t.TempDir())
	dbPath := seedOpenCodeV2DB(t, time.Now().UnixMilli())
	t.Setenv("WITNESS_OPENCODE_DB", dbPath)

	st, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	st.Close()

	if err := os.MkdirAll(filepath.Dir(opencode.PluginPath()), 0o700); err != nil {
		t.Fatal(err)
	}
	shimPath := filepath.Join(home, "witness.sh")
	if err := os.WriteFile(shimPath, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(opencode.PluginPath(), []byte(plugin.Source(shimPath)), 0o644); err != nil {
		t.Fatal(err)
	}

	oldVersion := openCodeVersion
	openCodeVersion = func(context.Context) (opencode.Version, error) {
		t.Fatal("version probed on status path")
		panic("unreachable")
	}
	t.Cleanup(func() { openCodeVersion = oldVersion })

	// Test 1: stale → warning with why line
	st, err = store.Open()
	if err != nil {
		t.Fatal(err)
	}
	st.SetMetaString("opencode_seen_time_updated_ms", fmt.Sprintf("%d", time.Now().Add(-1*time.Hour).UnixMilli()))
	st.Close()

	staleOut := captureStdout(t, func() { _ = cmdDistillStatus(false) })
	// Under the editor's name: "freshness: …" alone does not say whose capture is behind
	if !strings.Contains(staleOut, "opencode freshness: ") {
		t.Errorf("stale output does not name the editor; got:\n%s", staleOut)
	}
	if !strings.Contains(staleOut, "witness import --agent opencode") {
		t.Errorf("stale output missing import command; got:\n%s", staleOut)
	}
	if !strings.Contains(staleOut, "plugin may not be loading") {
		t.Errorf("stale output missing why line; got:\n%s", staleOut)
	}

	// Test 2: healthy → no OpenCode capture warning line
	st, err = store.Open()
	if err != nil {
		t.Fatal(err)
	}
	st.SetMetaString("opencode_seen_time_updated_ms", fmt.Sprintf("%d", time.Now().UnixMilli()))
	st.Close()

	healthyOut := captureStdout(t, func() { _ = cmdDistillStatus(false) })
	// Check for the capture health warning pattern, not just any mention of "opencode"
	if strings.Contains(healthyOut, "freshness:") || strings.Contains(healthyOut, "OpenCode has activity") {
		t.Errorf("healthy output has OpenCode capture warning when it should not; got:\n%s", healthyOut)
	}

	// Test 3: JSON unchanged
	jsonOut := captureStdout(t, func() { _ = cmdDistillStatus(true) })
	var report map[string]any
	if err := json.Unmarshal([]byte(jsonOut), &report); err != nil {
		t.Fatalf("JSON output not parseable: %v", err)
	}
	if _, ok := report["capture"]; ok {
		t.Errorf("JSON output has 'capture' key, should be unchanged")
	}
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
			home := t.TempDir()
			t.Setenv("WITNESS_HOME", home)
			t.Setenv("OPENCODE_CONFIG_DIR", t.TempDir())
			t.Setenv("OPENCODE_CONFIG", "")
			t.Setenv("WITNESS_PROMPTS", filepath.Join("..", "..", "prompts"))
			t.Setenv("WITNESS_OPENCODE_DB", seedOpenCodeV2DB(t, time.Now().UnixMilli()))
			prevEmb := newDoctorEmbedder
			newDoctorEmbedder = func() (distill.Embedder, error) { return fixedEmbedder{vec: []float32{1, 0, 0}}, nil }
			prevVer := openCodeVersion
			openCodeVersion = func(context.Context) (opencode.Version, error) {
				return opencode.Version{Major: 2, Minor: 2, Patch: 6}, nil
			}
			t.Cleanup(func() { newDoctorEmbedder, openCodeVersion = prevEmb, prevVer })

			st, err := store.Open()
			if err != nil {
				t.Fatal(err)
			}
			if err := configApplySet(st, "mine_model", "claude-haiku-4-5", ""); err != nil {
				t.Fatal(err)
			}
			if unwired {
				// Wired, imported once, then unwired
				if err := os.MkdirAll(filepath.Dir(opencode.PluginPath()), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(opencode.PluginPath(), []byte(plugin.Source(filepath.Join(home, "witness.sh"))), 0o644); err != nil {
					t.Fatal(err)
				}
				if err := st.SetMetaString("opencode_seen_time_updated_ms", fmt.Sprintf("%d", time.Now().Add(-time.Hour).UnixMilli())); err != nil {
					t.Fatal(err)
				}
			}
			st.Close()
			if unwired {
				captureStdout(t, func() { _ = cmdUninstall([]string{"opencode"}) })
			}

			st, err = store.Open()
			if err != nil {
				t.Fatal(err)
			}
			full, quick := openCodeHealth(st, false), openCodeHealth(st, true)
			st.Close()
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

// status runs on every call: a machine that has OpenCode but never wired it must not have its DB
// opened (a schema probe plus a scan of session_message) by `witness status`.
func TestStatusNeverOpensTheDatabaseOfAnUnwiredOpenCode(t *testing.T) {
	t.Setenv("WITNESS_HOME", t.TempDir())
	t.Setenv("OPENCODE_CONFIG_DIR", t.TempDir())
	t.Setenv("WITNESS_OPENCODE_DB", seedOpenCodeV2DB(t, time.Now().UnixMilli()))
	opens := 0
	prev := openCodeNewestActivity
	openCodeNewestActivity = func(ctx context.Context, path string) (time.Time, error) {
		opens++
		return prev(ctx, path)
	}
	t.Cleanup(func() { openCodeNewestActivity = prev })

	st, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	if checks := openCodeHealth(st, true); checks != nil {
		t.Errorf("quick checks = %+v, want none", checks)
	}
	st.Close()
	captureStdout(t, func() { _ = cmdDistillStatus(false) })
	if opens != 0 {
		t.Fatalf("status opened the OpenCode DB %d times for an OpenCode that was never wired, want 0", opens)
	}

	// The seam is the one health reads through: doctor still opens the DB to find the sessions
	st, err = store.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if openCodeHealth(st, false) == nil || opens == 0 {
		t.Fatalf("doctor found no sessions (opens = %d); the counting seam is not the one health uses", opens)
	}
}
