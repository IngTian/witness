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
//
// Internally Store is a thin facade over a set of focused, independently-testable
// concern types (issue #73-C1) — metaKV, and the others added alongside it — each
// holding only the shared *sql.DB (and, for the filesystem concerns, Root). Store
// EMBEDS them, so all of the original methods stay promoted onto *Store and every
// existing caller keeps compiling unchanged; the split is pure reorganization over
// one connection (MaxOpenConns(1)+WAL is unchanged — see openDB). Callers that only
// need a slice of the API can, at the decoupling seam, depend on a narrow interface
// instead of the whole Store. Root and db remain fields because Close/Export and the
// path helpers (ConfigPath/LogPath/dbPath) live directly on Store.
type Store struct {
	Root string
	db   *sql.DB

	metaKV     // small-scalar `meta` + `session_meta` bookkeeping (issue #73-C1)
	profileFS  // L4 narrative profile files under <root>/profile/ (issue #73-C1)
	procLocks  // cross-process advisory flocks (worker/import) (issue #73-C1)
	lensReg    // on-disk lens registry under <root>/lenses/ (issue #73-C1)
	facetIO    // L2 bi-temporal facets (reviewer is sole writer) (issue #73-C1)
	rawIO      // L0 append-only transcript + size/sample/prune helpers (issue #73-C1)
	obsIO      // L1 observations + in-session staged buffer (issue #73-C1)
	queue      // distillation queue: per-(session,lens) watermark/backoff (issue #73-C1)
	configFile // config.toml + review cadence + runner-bound flag (issue #73-C1)
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
	// The filesystem concerns get their own copy of root (immutable post-Open, so it
	// can't drift from Store.Root). configFile is wired with root now so the pre-openDB
	// EnsureConfigFile below resolves the right path; its db handle is filled in after
	// openDB. The purely DB-backed concerns are wired after openDB.
	s := &Store{
		Root:       root,
		profileFS:  profileFS{root: root},
		procLocks:  procLocks{root: root},
		lensReg:    lensReg{root: root},
		configFile: configFile{root: root},
	}
	// Lay down a fully-commented config template on first contact so any command a
	// user runs (doctor, profile, lens...) exposes every tunable — not just the
	// fields install writes. Existing configs are never touched (forward-compatible).
	// Best-effort: a write failure is ignored because config is optional and a
	// command must not fail just because it couldn't write a template. Uses only the
	// config path (root), so it is safe before the db handle is wired below.
	_ = s.EnsureConfigFile()
	db, err := openDB(s.dbPath())
	if err != nil {
		return nil, fmt.Errorf("open db: %w", err)
	}
	s.db = db
	// Every DB-backed concern shares this single handle (the deliberate
	// MaxOpenConns(1)+WAL model). Wire them here, after openDB, and before the
	// one-shot steps below that call promoted methods (adoptRunnerBound reads the
	// runner-bound flag via the metaKV-backed MetaString).
	s.metaKV = metaKV{db: db}
	s.facetIO = facetIO{db: db}
	s.rawIO = rawIO{db: db}
	s.obsIO = obsIO{db: db}
	s.queue = queue{db: db}
	s.configFile.db = db // root was set in the literal above (for EnsureConfigFile)
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
	// One-shot (#44 slice 1a + #102): on the FIRST open of an archive that wants it — a
	// brand-new personal install OR one that predates 1a (ran under the old always-on
	// "default") — auto-seed "default" as an ordinary registered+enabled lens, so a fresh
	// user gets a working setup and an upgrade loses nothing. It is an INJECTED hook
	// (SeedDefaultLens), not an inline step, because seeding copies the BUNDLED default
	// prompts into the registry, which needs internal/lens + internal/bundle — and store,
	// the bottom of the stack, imports nothing internal. cmd wires it at startup; nil in
	// pure-store/test contexts (correct: no bundle there, nothing to seed). Idempotent +
	// one-shot-gated inside the hook, so a deliberately-removed default stays gone.
	if SeedDefaultLens != nil {
		SeedDefaultLens(s)
	}
	return s, nil
}

// SeedDefaultLens, when set by the cmd layer at startup, is invoked once at the end of
// Open to auto-seed the built-in "default" lens the first time an archive is opened that
// wants it: a fresh personal install (empty archive, empty registry) or one that predates
// #44 slice 1a (distilled under the old always-on default). It is a package var
// (dependency injection) rather than an inline Open step because the seed source is the
// bundled prompts dir, reachable only from internal/lens/internal/bundle; store must not
// import them. The hook owns its own idempotency, its "should this archive get default?"
// gate (see HasLegacyDefaultData + IsEmptyArchive), and a durable one-shot marker so a
// deleted default is never resurrected (#102 "default is deletable"). Left nil, Open does
// no seeding — the correct behavior for tests and any store-only consumer.
var SeedDefaultLens func(*Store)

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

func (s *Store) ConfigPath() string { return configTomlPath(s.Root) }
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
