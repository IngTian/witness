package store

import (
	"fmt"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"
)

// The two valid distillation runners. Defined here (the config layer that owns the
// `runner` field) so CLI validation and the config template share one source of truth,
// without importing internal/platform (which would invert the store→platform layering).
const (
	RunnerClaude   = "claude"
	RunnerOpenCode = "opencode"
)

// Config holds the user-tunable knobs. Parsed from a tiny line-based config.toml
// (key = value); we avoid a TOML dependency to keep the binary lean. Unknown
// keys are ignored; missing file = all defaults.
type Config struct {
	Runner          string // "claude" (default) or "opencode" for headless distillation calls
	TriageModel     string // model for cheap per-session mining ("" = claude -p default, e.g. on Bedrock)
	DistillModel    string // model for the reviewer ("" = claude -p default)
	ReviewEvery     int    // run the reviewer after this many distilled sessions since last review
	ReviewPoignancy int    // ...OR when accumulated observation poignancy since last review crosses this (0 = disabled)
	AutoDistill     bool   // whether hooks/plugins may start the worker automatically
	MineConcurrency int    // max sessions mined in parallel per drain; the engine clamps this to GOMAXPROCS and to 1 when the runner is not ConcurrentRunSafe (issue #22). <=0 means DefaultMineConcurrency.
	// EnabledLenses is the set of registered lens names that run on EVERY session
	// (alongside the always-on "default" lens). Lenses are global and centrally
	// registered — not tied to a repo path — so the same lens is shared everywhere.
	EnabledLenses []string
}

// DefaultMineConcurrency is the default cap on sessions mined in parallel per
// drain when mine_concurrency is unset. Chosen for a laptop: the embedder loads
// once and is shared (~1.5GB), each concurrent `claude -p` adds ~0.35GB, so 4
// peaks around 2.9GB. The engine additionally clamps to GOMAXPROCS and to 1 for a
// runner that is not ConcurrentRunSafe.
const DefaultMineConcurrency = 4

func DefaultConfig() Config {
	return Config{
		Runner:          "claude",
		TriageModel:     "", // empty => let `claude -p` use the environment default model
		DistillModel:    "",
		ReviewEvery:     5,
		ReviewPoignancy: 30, // a few high-salience sessions trigger review before the count cap
		// Automatic triggers stay laptop-friendly WITHOUT a cooldown: capture is
		// immediate, the machine-wide WorkerLock single-flights the worker (extra
		// triggers no-op in ms), and the worker drains everything then exits so the
		// embed model never stays resident.
		AutoDistill:     true,
		MineConcurrency: DefaultMineConcurrency,
	}
}

// LoadConfig reads config.toml if present, layering over defaults.
func (s *Store) LoadConfig() Config {
	c := DefaultConfig()
	data, err := os.ReadFile(s.ConfigPath())
	if err != nil {
		return c
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		k = strings.TrimSpace(k)
		v = strings.Trim(strings.TrimSpace(v), `"`)
		switch k {
		case "runner":
			if v != "" {
				c.Runner = v
			}
		case "triage_model":
			c.TriageModel = v
		case "distill_model":
			c.DistillModel = v
		case "review_every":
			// Clamp <=0 back to the default rather than accepting it verbatim (issue
			// #49 I1). ReviewDue tests `SessionsSinceReview() >= ReviewEvery`, so a 0
			// or negative value would make a review ALWAYS due — firing the reviewer +
			// full L4 regen on essentially every session-end, the opposite of the
			// "0 = off" a user might infer from review_poignancy. Mirrors the
			// mine_concurrency guard below. (To disable count-based review, raise the
			// number; the poignancy trigger is the one with off-via-0 semantics.)
			if n, err := strconv.Atoi(v); err == nil {
				if n <= 0 {
					c.ReviewEvery = DefaultConfig().ReviewEvery
				} else {
					c.ReviewEvery = n
				}
			}
		case "review_poignancy":
			if n, err := strconv.Atoi(v); err == nil {
				c.ReviewPoignancy = n
			}
		case "auto_distill":
			if b, ok := parseBool(v); ok {
				c.AutoDistill = b
			}
		// auto_distill_interval_minutes / auto_distill_session_budget were the
		// pre-#22 throughput throttles (a start cooldown + a per-run session cap).
		// They are gone: WorkerLock single-flights the worker and it self-drains via
		// its re-check loop, so throttling WHEN it starts bought nothing but the 1 Hz
		// wakeup cascade. Old config lines are harmlessly ignored (unknown keys are).
		case "mine_concurrency":
			// <=0 restores the default rather than disabling mining (0 goroutines
			// would drain nothing); the engine still clamps the effective value.
			if n, err := strconv.Atoi(v); err == nil {
				if n <= 0 {
					c.MineConcurrency = DefaultMineConcurrency
				} else {
					c.MineConcurrency = n
				}
			}
		case "lens":
			// One enabled lens per line: "lens = <name>". Global — runs on every
			// session. Deduped so repeated lines don't multiply.
			if v != "" && !slices.Contains(c.EnabledLenses, v) {
				c.EnabledLenses = append(c.EnabledLenses, v)
			}
		}
	}
	return c
}

// runnerBoundKey is the SINGLE source of truth for "did the user explicitly bind a
// distillation runner?" (issue #71). "1" means bound → the config runner wins over
// any WITNESS_RUNNER env fallback; unset means unbound → the env fallback applies.
//
// It used to be one of THREE reconciled signals (this flag, a template marker
// comment, and a live-scan for an active `runner=` line), which forced a fragile
// special-case in setConfigKey and let a routine `config set triage_model` perturb
// runner resolution. Now the flag alone decides, and adoptRunnerBound (run once at
// Open) folds the two "config already says bound" populations into the flag so
// resolution never re-reads config text.
const runnerBoundKey = "runner_bound"

// ResolveRunner returns the distillation runner to actually use, layering a
// non-persistent WITNESS_RUNNER env fallback UNDER any explicit config choice.
//
// Why this exists: the npm OpenCode plugin user never runs `witness install`, so
// their config.toml carries the template default runner="claude" — but they have
// no `claude` CLI, and distillation would silently fail. The plugin passes
// WITNESS_RUNNER=opencode so the worker it kicks distills via OpenCode instead.
//
// Precedence (safety-first, so a dual CC+OpenCode user is never hijacked):
//  1. If a runner is bound (runnerBoundKey="1"), the config value ALWAYS wins —
//     WITNESS_RUNNER is ignored.
//  2. Else, if WITNESS_RUNNER is set, use it (the plugin fallback).
//  3. Else, the config/default value.
//
// The bound flag is the ONLY state read here — never config text — so no config
// write (e.g. `config set triage_model`) can ever change what runner resolves.
// adoptRunnerBound (at Open) is what stamps the flag for a config that already
// carries a deliberate runner choice.
func (s *Store) ResolveRunner(cfg Config) string {
	if s.MetaString(runnerBoundKey) == "1" {
		return cfg.Runner
	}
	if env := strings.TrimSpace(os.Getenv("WITNESS_RUNNER")); env != "" {
		return env
	}
	return cfg.Runner
}

// adoptRunnerBound is a one-time reconciliation run at Open: if the bound flag is
// unset AND config.toml carries a deliberate runner choice — an ACTIVE (uncommented)
// `runner=` line — stamp the flag so resolution treats it as bound WITHOUT ever
// re-reading config text again. This folds the two pre-#71 "config says bound"
// populations into the flag: a legacy markerless config (an old install that wrote
// runner=), and a user who manually uncommented the template's runner line.
//
// A COMMENTED template line is NOT adopted: the npm OpenCode-plugin user (who never
// ran install) keeps an unbound flag and stays resolved via WITNESS_RUNNER. Best-
// effort and idempotent (matching the sibling one-shot Open steps): once the flag is
// "1" this is a no-op; a config read error leaves the flag unset (env fallback still
// applies); and a stamp WRITE failure (only reachable under a >5s busy-timeout or a
// dead disk, which fail Open outright) simply retries on the next Open, so a legacy
// active-line archive self-heals to bound — no persistent misresolution.
func (s *Store) adoptRunnerBound() {
	if s.MetaString(runnerBoundKey) == "1" {
		return // already bound (via install/config set, or a prior adoption)
	}
	data, err := os.ReadFile(s.ConfigPath())
	if err != nil {
		return
	}
	for _, line := range strings.Split(string(data), "\n") {
		if isConfigKeyLine(line, "runner") {
			_ = s.SetMetaString(runnerBoundKey, "1")
			return
		}
	}
}

func parseBool(v string) (bool, bool) {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on":
		return true, true
	case "0", "false", "no", "off":
		return false, true
	default:
		return false, false
	}
}

// SetRunner writes `runner = "<runner>"` into config.toml, creating or replacing
// the existing line. Used by `install` to bind the distillation runtime to the
// integration that was just wired (Claude Code → claude, OpenCode → opencode) so
// the user does not have to hand-edit config. Other lines (comments, lenses,
// other keys) are preserved verbatim. The value is quoted to match the format
// EnsureConfigFile writes and to stay consistent with other string fields.
func (s *Store) SetRunner(runner string) error {
	if err := s.setConfigKey("runner", runner); err != nil {
		return err
	}
	// Mark that a runner was explicitly chosen via install, so ResolveRunner lets
	// this persisted value win over any WITNESS_RUNNER env fallback (a dual
	// CC+OpenCode user who ran `install` is never hijacked by the plugin env).
	return s.SetMetaString(runnerBoundKey, "1")
}

// SetConfigString sets a string-valued config.toml key (creating or replacing its
// line, preserving everything else), the CLI-facing counterpart to hand-editing the
// file — used by `witness config`. Marks the runner as explicitly bound when key is
// "runner" so a CLI-set runner wins over the WITNESS_RUNNER env fallback exactly like
// `install` does. Other keys don't need the marker.
func (s *Store) SetConfigString(key, value string) error {
	if err := s.setConfigKey(key, value); err != nil {
		return err
	}
	if key == "runner" {
		return s.SetMetaString(runnerBoundKey, "1")
	}
	return nil
}

// setConfigKey is the shared line-rewrite for a quoted string key in config.toml:
// replace the FIRST occurrence of `<key> = ...` in place (dropping any duplicates),
// else append it; comments, blank lines, and every other key are preserved verbatim.
// The value is quoted to match EnsureConfigFile's format.
//
// It NEVER touches runner-resolution state (issue #71): the bound flag is stamped
// explicitly by SetRunner/SetConfigString for the runner key, and ResolveRunner
// reads only that flag — never config text — so a model-key write here cannot
// perturb which runner resolves. (Previously this had to special-case a marker
// comment to avoid exactly that coupling.)
func (s *Store) setConfigKey(key, value string) error {
	data, err := os.ReadFile(s.ConfigPath())
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	newLine := fmt.Sprintf("%s = %q", key, value)
	var kept []string
	set := false
	if len(data) > 0 {
		for _, line := range strings.Split(string(data), "\n") {
			if isConfigKeyLine(line, key) {
				if !set {
					kept = append(kept, newLine)
					set = true
				}
				continue // drop any duplicate lines for this key
			}
			kept = append(kept, line)
		}
	}
	for len(kept) > 0 && strings.TrimSpace(kept[len(kept)-1]) == "" {
		kept = kept[:len(kept)-1]
	}
	if !set {
		kept = append(kept, newLine)
	}
	out := strings.Join(kept, "\n") + "\n"
	return writeAtomic(s.ConfigPath(), []byte(out))
}

// EnsureConfigFile creates config.toml with a full commented template if it does
// not yet exist, so a first-time install exposes every tunable (not just runner)
// and the user can see what to edit. Existing files are never overwritten —
// later installs only refresh `runner` via SetRunner, leaving user edits intact.
// Forward-compatible: old configs without some fields simply fall back to defaults.
func (s *Store) EnsureConfigFile() error {
	if _, err := os.Stat(s.ConfigPath()); err == nil {
		return nil // already present; never clobber user edits
	} else if !os.IsNotExist(err) {
		return err
	}
	tpl := `# witness configuration — all fields optional, shown with defaults.
# Docs: https://github.com/IngTian/witness#configuration

# Distillation runtime: "claude" (default, uses ` + "`claude -p`" + `) or "opencode"
# (uses ` + "`opencode serve`" + `). Uncomment to bind manually; ` + "`witness install`" + ` also binds it.
# runner = "claude"

# Models for the per-session miner and the periodic reviewer. Empty = use the
# ` + "`claude -p`" + ` / ` + "`opencode run`" + ` default. With runner = opencode, use OpenCode
# model names such as "openai/gpt-5.5".
triage_model  = ""
distill_model = ""

# Run the reviewer after this many distilled sessions since the last review.
# Must be >= 1 (a value <= 0 is treated as the default, 5 — it does NOT disable
# review; to review less often, raise this number).
review_every = 5
# ...or once accumulated observation poignancy crosses this threshold (0 = off).
review_poignancy = 30

# Automatic distillation is laptop-friendly without keeping the embed model
# resident: hooks capture immediately, a single-flight lock ensures just one worker
# runs, and it drains the whole queue (re-checking for new work as it goes) then
# exits. Set false to distill only on demand via ` + "`witness distill start`" + `.
auto_distill = true

# Sessions mined in parallel per drain (backfill speed). The embedder loads once
# and is shared; each concurrent distillation call adds ~0.35GB. Clamped to the
# CPU count and to 1 for a runner that can't run concurrently. <=0 = default (4).
mine_concurrency = 4

# Enabled lenses (one per line). Managed by ` + "`witness lens enable/disable <name>`" + `.
# lens = math
`
	return writeAtomic(s.ConfigPath(), []byte(tpl))
}

// isConfigKeyLine reports whether a config line is a `<key> = ...` assignment for the
// given key (a real assignment, not a comment or blank line).
func isConfigKeyLine(line, key string) bool {
	t := strings.TrimSpace(line)
	if t == "" || strings.HasPrefix(t, "#") {
		return false
	}
	k, _, ok := strings.Cut(t, "=")
	return ok && strings.TrimSpace(k) == key
}

// --- review cadence ----------------------------------------------------------
//
// Both signals are read straight from the DB instead of scanning files or the
// whole observation corpus. A review records two offsets in `meta`:
//   - review_obs_rowid: the max observation rowid at review time. Poignancy since
//     review is SUM(poignancy) for rowids beyond it — an O(log n) indexed scan,
//     not a full corpus read + ts parse on the hot path.
//   - review_ts: when the review ran. Sessions since review = distilled sessions
//     whose distilled_at is later (RFC3339 UTC sorts lexically).

// StampReview records that a review just ran by advancing both review offsets.
func (s *Store) StampReview() error {
	now := time.Now().UTC().Format(time.RFC3339)
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	var maxRow int64
	if err := tx.QueryRow(`SELECT COALESCE(MAX(rowid), 0) FROM observations`).Scan(&maxRow); err != nil {
		tx.Rollback()
		return err
	}
	upsert := `INSERT INTO meta(key, value) VALUES (?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value`
	if _, err := tx.Exec(upsert, "review_obs_rowid", strconv.FormatInt(maxRow, 10)); err != nil {
		tx.Rollback()
		return err
	}
	if _, err := tx.Exec(upsert, "review_ts", now); err != nil {
		tx.Rollback()
		return err
	}
	return tx.Commit()
}

// SessionsSinceReview counts sessions distilled since the last review stamp.
// Progress is per-(session,lens), so COUNT DISTINCT session — else a session mined
// by N lenses would count N times and trip the review cadence N× too early.
func (s *Store) SessionsSinceReview() int {
	last := s.metaStr("review_ts")
	var n int
	_ = s.db.QueryRow(
		`SELECT COUNT(DISTINCT session) FROM progress WHERE distilled_at != '' AND distilled_at > ?`, last).Scan(&n)
	return n
}

// PoignancySinceReview sums the poignancy of observations recorded since the last
// review. This is the salience signal: a few high-poignancy sessions can trigger
// a review sooner than the plain session-count cap would.
func (s *Store) PoignancySinceReview() int {
	off := s.metaInt("review_obs_rowid")
	var sum int
	_ = s.db.QueryRow(
		`SELECT COALESCE(SUM(poignancy), 0) FROM observations WHERE rowid > ?`, off).Scan(&sum)
	return sum
}

// --- small scalar bookkeeping (meta table) -----------------------------------

func (s *Store) metaStr(key string) string {
	var v string
	_ = s.db.QueryRow(`SELECT value FROM meta WHERE key = ?`, key).Scan(&v)
	return v
}

// MetaString exposes small scalar bookkeeping to importers that need their own
// durable watermarks without owning schema migrations.
func (s *Store) MetaString(key string) string { return s.metaStr(key) }

// SetMetaString stores a small scalar watermark under key.
func (s *Store) SetMetaString(key, value string) error {
	_, err := s.db.Exec(
		`INSERT INTO meta(key, value) VALUES (?, ?)
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value`, key, value)
	return err
}

// Drift-surfacing meta keys (issue #57). A distillation pass records prose_drift —
// a lens whose model returned no JSON observation array, likely a below-floor triage
// model — so `witness doctor` and a backfill's completion line can surface it. These
// live in the existing worker_*/review_* meta namespace (no schema, no collision).
// The total is monotonic across the archive's life; the last_* stamps give doctor a
// "when/where" so a raised model reads as "0 recent", not "broken forever".
const (
	metaDriftTotal       = "mine_drift_total"
	metaDriftLastTS      = "mine_drift_last_ts"
	metaDriftLastSession = "mine_drift_last_session"
	metaDriftLastLens    = "mine_drift_last_lens"
)

// RecordDrift atomically adds n to the drift counter and stamps the most recent drift
// (ts/session/lens), in ONE transaction so a concurrent reader never sees the counter
// bumped without its stamps. Called by the sole L1 writer (CommitMining), so there is
// no cross-process race on the counter beyond what the single-writer model already
// serializes. Best-effort at the call site: a failure here never fails the commit.
func (s *Store) RecordDrift(n int, session, lens string) error {
	if n <= 0 {
		return nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	// Atomic increment: read-modify-write inside the tx would still be correct under
	// MaxOpenConns(1), but expressing it as one UPDATE keeps it a single statement and
	// robust if the connection model ever loosens.
	if _, err := tx.Exec(
		`INSERT INTO meta(key, value) VALUES (?, ?)
		 ON CONFLICT(key) DO UPDATE SET value = CAST(meta.value AS INTEGER) + excluded.value`,
		metaDriftTotal, strconv.Itoa(n)); err != nil {
		tx.Rollback()
		return err
	}
	stamps := [][2]string{
		{metaDriftLastTS, time.Now().UTC().Format(time.RFC3339)},
		{metaDriftLastSession, session},
		{metaDriftLastLens, lens},
	}
	for _, kv := range stamps {
		if _, err := tx.Exec(
			`INSERT INTO meta(key, value) VALUES (?, ?)
			 ON CONFLICT(key) DO UPDATE SET value = excluded.value`, kv[0], kv[1]); err != nil {
			tx.Rollback()
			return err
		}
	}
	return tx.Commit()
}

// DriftTotal is the monotonic count of prose_drift events recorded so far (0 if never).
func (s *Store) DriftTotal() int {
	n, _ := strconv.Atoi(s.metaStr(metaDriftTotal))
	return n
}

// DriftLast returns the timestamp and lens of the most recently recorded drift event
// (both "" if none) — for doctor's "last drift" line so a monotonic counter reads as
// dated, not perpetually-broken.
func (s *Store) DriftLast() (ts, lens string) {
	return s.metaStr(metaDriftLastTS), s.metaStr(metaDriftLastLens)
}

func (s *Store) metaInt(key string) int64 {
	n, _ := strconv.ParseInt(strings.TrimSpace(s.metaStr(key)), 10, 64)
	return n
}

// ReviewDue reports whether the reviewer should run: either enough distilled
// sessions since the last review (the cap), OR enough accumulated poignancy
// (salience) — whichever comes first.
func (s *Store) ReviewDue(cfg Config) bool {
	if s.SessionsSinceReview() >= cfg.ReviewEvery {
		return true
	}
	return cfg.ReviewPoignancy > 0 && s.PoignancySinceReview() >= cfg.ReviewPoignancy
}

// --- enabled-lens writers (global) -------------------------------------------

// EnableLens adds a lens name to the globally-enabled set (idempotent). It runs
// on every session thereafter. Rewrites config.toml, preserving all other lines.
// A reserved name (the always-on built-in / the unified summary) is refused:
// belt-and-suspenders alongside RegisterLens, so even a config hand-edit that
// slipped a reserved name into the registry can't enable it into the active set.
func (s *Store) EnableLens(name string) error {
	if ReservedLensName(name) {
		return fmt.Errorf("lens name %q is reserved and cannot be enabled (the built-in %q lens always runs)", name, LensDefault)
	}
	return s.rewriteEnabledLens(name, true)
}

// DisableLens removes a lens from the enabled set (no-op if absent).
func (s *Store) DisableLens(name string) error { return s.rewriteEnabledLens(name, false) }

// rewriteEnabledLens drops any existing `lens = <name>` line for name, then (if
// enabling) appends one. Read-modify-write of the whole file, atomic on rename —
// fine at config scale. Comments and other settings are preserved verbatim.
func (s *Store) rewriteEnabledLens(name string, enable bool) error {
	data, err := os.ReadFile(s.ConfigPath())
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	var kept []string
	if len(data) > 0 {
		for _, line := range strings.Split(string(data), "\n") {
			if n, ok := lensLineName(line); ok && n == name {
				continue // drop the existing entry for this name
			}
			kept = append(kept, line)
		}
	}
	for len(kept) > 0 && strings.TrimSpace(kept[len(kept)-1]) == "" {
		kept = kept[:len(kept)-1]
	}
	if enable {
		kept = append(kept, fmt.Sprintf("lens = %s", name))
	}
	out := strings.Join(kept, "\n")
	if out != "" {
		out += "\n"
	}
	return writeAtomic(s.ConfigPath(), []byte(out))
}

// lensLineName returns the lens name in a `lens = <name>` config line, or
// ok=false for any other (or commented) line.
func lensLineName(line string) (string, bool) {
	t := strings.TrimSpace(line)
	if t == "" || strings.HasPrefix(t, "#") {
		return "", false
	}
	k, v, ok := strings.Cut(t, "=")
	if !ok || strings.TrimSpace(k) != "lens" {
		return "", false
	}
	return strings.Trim(strings.TrimSpace(v), `"`), true
}
