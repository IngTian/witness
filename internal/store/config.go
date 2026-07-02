package store

import (
	"fmt"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"
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
	// EnabledLenses is the set of registered lens names that run on EVERY session
	// (alongside the always-on "default" lens). Lenses are global and centrally
	// registered — not tied to a repo path — so the same lens is shared everywhere.
	EnabledLenses []string
}

func DefaultConfig() Config {
	return Config{
		Runner:          "claude",
		TriageModel:     "", // empty => let `claude -p` use the environment default model
		DistillModel:    "",
		ReviewEvery:     5,
		ReviewPoignancy: 30, // a few high-salience sessions trigger review before the count cap
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
			if n, err := strconv.Atoi(v); err == nil {
				c.ReviewEvery = n
			}
		case "review_poignancy":
			if n, err := strconv.Atoi(v); err == nil {
				c.ReviewPoignancy = n
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
func (s *Store) SessionsSinceReview() int {
	last := s.metaStr("review_ts")
	var n int
	_ = s.db.QueryRow(
		`SELECT COUNT(*) FROM progress WHERE distilled_at != '' AND distilled_at > ?`, last).Scan(&n)
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
func (s *Store) EnableLens(name string) error { return s.rewriteEnabledLens(name, true) }

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
