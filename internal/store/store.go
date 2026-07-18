package store

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// dataDirNames lists the app's per-user data directory names in PREFERENCE order:
// the current name first, then any legacy names, newest to oldest. When no data
// dir yet exists, the first entry ("witness") is created; when one already exists
// under a legacy name, it is adopted IN PLACE (no move) so the repo rename never
// orphans an existing archive. A future rename just prepends to this list.
//
// The rename history: the tool shipped as "claude-witness" and the repo/command
// later became "witness"; the data dir lagged. Rather than force a risky move of
// live SQLite data, resolution adopts whichever name is already present.
var dataDirNames = []string{"witness", "claude-witness"}

// Store is the per-user data root. Data lives under $XDG_DATA_HOME/<name> (default
// ~/.local/share/<name>), where <name> is resolved from dataDirNames — the app
// owns its own namespace rather than nesting in Claude Code's ~/.claude config
// home. Override the whole root with WITNESS_HOME.
//
// The data layers (raw turns, observations, bi-temporal facets) plus the
// distillation queue live in a single SQLite database (witness.db). The derived
// profile summaries (profile/*.md), user-authored config (config.toml), and lens
// definitions (lenses/<name>/ — a lens.json plus extract.md/review.md, issue #75)
// stay as plain files, since they are meant to be read (and, for config/lenses,
// edited) directly.
type Store struct {
	Root string
	db   *sql.DB
}

// Open returns the Store rooted at WITNESS_HOME, else the resolved default under
// $XDG_DATA_HOME (or ~/.local/share), creating the root and opening (migrating)
// the database. Callers should Close when done so WAL state is flushed cleanly.
func Open() (*Store, error) {
	root, err := resolveRoot()
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(root, 0o700); err != nil { // 0700: private growth data
		return nil, fmt.Errorf("mkdir %s: %w", root, err)
	}
	s := &Store{Root: root}
	// Lay down a fully-commented config template on first contact so any command a
	// user runs (doctor, profile, lens...) exposes every tunable — not just the
	// fields install writes. Existing configs are never touched (forward-compatible).
	// Best-effort: a write failure is ignored because config is optional and a
	// command must not fail just because it couldn't write a template.
	_ = s.EnsureConfigFile()
	db, err := openDB(s.dbPath())
	if err != nil {
		return nil, fmt.Errorf("open db: %w", err)
	}
	s.db = db
	// One-shot (issue #71): fold a config that already carries a deliberate runner
	// choice (a legacy markerless install, or a manually-uncommented runner line)
	// into the runner_bound flag — the SINGLE source of truth ResolveRunner reads.
	// Idempotent + best-effort (see adoptRunnerBound); needs s.db, so it runs after
	// openDB. Keeps the "is a runner bound?" decision out of every config read.
	s.adoptRunnerBound()
	// One-shot: convert any pre-#75 single-file lens.md registry dirs to the new
	// lens.json + extract.md + review.md format (issue #75). Idempotent + non-destructive
	// (see migrateLegacyLenses); best-effort like the config template — a filesystem
	// hiccup must not fail an Open. Keeps legacy handling in ONE frozen place (lensmigrate.go)
	// instead of scattering old-format checks across the codebase, mirroring the DB migration.
	_ = s.migrateLegacyLenses()
	return s, nil
}

// resolveRoot picks the data-root directory. WITNESS_HOME, if set, wins verbatim
// (explicit user intent — used by tests and power users). Otherwise the root is
// <base>/<name>, where base is $XDG_DATA_HOME or ~/.local/share and <name> is
// resolved from dataDirNames: adopt the FIRST name whose directory already exists
// (so an archive created under a legacy name keeps being used IN PLACE — nothing
// is moved), else fall back to dataDirNames[0] (the current preferred name) for a
// fresh install. This is why the rename can never orphan data.
func resolveRoot() (string, error) {
	if home := os.Getenv("WITNESS_HOME"); home != "" {
		return home, nil
	}
	base := os.Getenv("XDG_DATA_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("resolve home: %w", err)
		}
		base = filepath.Join(home, ".local", "share")
	}
	for _, name := range dataDirNames {
		cand := filepath.Join(base, name)
		if info, err := os.Stat(cand); err == nil && info.IsDir() {
			return cand, nil // adopt an existing dir in place (current or legacy)
		}
	}
	return filepath.Join(base, dataDirNames[0]), nil // fresh install: preferred name
}

// Close releases the database handle (and flushes the WAL).
// Close is idempotent: a second call is a no-op, not a double-close error. Some
// paths close the store early (e.g. `lens backfill` closes before handing off to a
// fresh worker store) while a defer will also fire — nil-ing the handle makes both
// safe.
func (s *Store) Close() error {
	if s.db != nil {
		db := s.db
		s.db = nil
		return db.Close()
	}
	return nil
}

// --- file paths for user-authored, non-DB state ------------------------------

func (s *Store) ConfigPath() string { return filepath.Join(s.Root, "config.toml") }
func (s *Store) LogPath() string    { return filepath.Join(s.Root, "witness.log") }

// sanitize keeps lens-derived filenames safe (names are short slugs, but be strict).
func sanitize(id string) string {
	id = strings.TrimSpace(id)
	if id == "" {
		id = "unknown"
	}
	var b strings.Builder
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
	}
	return b.String()
}

// writeAtomic writes data to path via a temp file + rename (atomic on POSIX).
// Used for config.toml rewrites; the data layers are transactional in SQLite.
func writeAtomic(path string, data []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
