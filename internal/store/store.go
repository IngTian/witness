package store

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Store is the per-user data root. Data lives under $XDG_DATA_HOME/claude-witness
// (default ~/.local/share/claude-witness) — the app owns its own namespace rather
// than nesting in Claude Code's ~/.claude config home. Override with WITNESS_HOME.
//
// The data layers (raw turns, observations, bi-temporal facets) plus the
// distillation queue live in a single SQLite database (witness.db). The derived
// profile summaries (profile/*.md), user-authored config (config.toml), and lens
// definitions (lenses/<name>/lens.md) stay as plain files, since they are meant to
// be read (and, for config/lenses, edited) directly.
type Store struct {
	Root string
	db   *sql.DB
}

// Open returns the Store rooted at WITNESS_HOME, else $XDG_DATA_HOME/claude-witness,
// else ~/.local/share/claude-witness, creating the root and opening (migrating)
// the database. Callers should Close when done so WAL state is flushed cleanly.
func Open() (*Store, error) {
	root := os.Getenv("WITNESS_HOME")
	if root == "" {
		if xdg := os.Getenv("XDG_DATA_HOME"); xdg != "" {
			root = filepath.Join(xdg, "claude-witness")
		} else {
			home, err := os.UserHomeDir()
			if err != nil {
				return nil, fmt.Errorf("resolve home: %w", err)
			}
			root = filepath.Join(home, ".local", "share", "claude-witness")
		}
	}
	if err := os.MkdirAll(root, 0o700); err != nil { // 0700: private growth data
		return nil, fmt.Errorf("mkdir %s: %w", root, err)
	}
	s := &Store{Root: root}
	db, err := openDB(s.dbPath())
	if err != nil {
		return nil, fmt.Errorf("open db: %w", err)
	}
	s.db = db
	return s, nil
}

// Close releases the database handle (and flushes the WAL).
func (s *Store) Close() error {
	if s.db != nil {
		return s.db.Close()
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
