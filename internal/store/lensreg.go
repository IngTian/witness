package store

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// ReservedLensName reports whether a lens name is reserved and may not be taken by
// a registered lens. Since #44 slice 1a exactly ONE name is reserved:
//   - ProfileUnified ("unified") — the cross-lens profile summary's filename stem; a
//     per-lens summary under this name would clobber the unified portrait (profile/
//     unified.md). It is NOT a lens (nothing mines it), so reserving its name can't
//     deny a user any real lens.
//
// "default" is NO LONGER reserved: it is an ordinary registered lens (the person-growth
// scaffold witness seeds). A user may re-register "default" with their own prompts —
// that intentionally redefines their own lens; it doesn't collide with anyone else's.
//
// Since #44 slice 1a "default" is NO LONGER reserved: it is now an ordinary
// registered lens (the personal-growth scaffold witness ships and seeds on a fresh
// tool install), freely enable/disable/edit/re-registerable like any other. The
// engine already treated every lens as just a prompt + a name; dropping default's
// reservation removes the last identity-layer specialness so an install can run any
// lenses it wants (including none).
//
// "unified" STAYS reserved: it is not a lens at all but the filename stem of the
// cross-lens profile portrait (profile/unified.md). A per-lens summary registered
// under that name would clobber the unified portrait. The check is on the sanitized
// name (registry filesystem key), case-FOLDED, because on the case-insensitive
// filesystems witness's primary platforms use (macOS APFS, Windows NTFS)
// profile/Unified.md and profile/unified.md are the SAME file — a case-sensitive
// check would let `register Unified` through and silently clobber the portrait.
func ReservedLensName(name string) bool {
	return strings.ToLower(sanitize(name)) == ProfileUnified
}

// lensReg is the lens-registry concern: the on-disk lens definitions under
// <root>/lenses/<name>/ (each a directory of lens.json + extract.md + review.md,
// issue #75). A filesystem leaf — it holds only the data root and never touches the DB. Its
// registry-mutation lock rides the shared flockPath primitive, so it depends on no
// other concern.
type lensReg struct{ root string }

// LensesDir is the central lens registry: <root>/lenses/<name>/ (each a directory of
// lens.json + extract.md + review.md, issue #75). Lenses live here (not in repos) so
// the same definition is shared across all sessions.
func (r *lensReg) LensesDir() string { return filepath.Join(r.root, "lenses") }

// errLensBusy is returned when a registry-mutating op can't take the registry lock
// because another one holds it (a rare interactive collision; retry).
var errLensBusy = fmt.Errorf("another lens registry operation is in progress; retry")

// lensRegistryLock single-flights registry-directory MUTATIONS (RegisterLens,
// SetLensModel) so two concurrent ops can't interleave through the shared staging
// path and lose a lens. It is a filesystem flock independent of WorkerLock (a worker
// drain and a lens edit are unrelated), non-blocking (LOCK_EX|LOCK_NB) like the others.
func (r *lensReg) lensRegistryLock() (unlock func(), ok bool) {
	return flockPath(r.root, ".lens-registry.lock")
}

// The lens-directory format (issue #75), defined ONCE. The store is the bottom of the stack,
// so it owns the format and internal/lens aliases it. The registry used to keep its own
// literal copies "in sync by comment", and merge a third list: that drift is how an optional
// prompt the loader read went uncopied by RegisterLens from v0.6.0 to v0.7.3, and the lens
// silently fell back to review.md.
const (
	LensConfigFile  = "lens.json"  // settings; optional
	LensExtractFile = "extract.md" // the mining prompt; the one required file
	LensReviewFile  = "review.md"  // the review prompt; optional
)

// LensFile is one file of a lens directory.
type LensFile struct {
	Name     string
	Required bool // a lens directory without it is not a lens
}

// LensFiles is EVERY file a lens directory consists of: RegisterLens copies exactly these,
// merge compares exactly these, and the loader reads nothing else
// (internal/lens TestLoadDirReadsOnlyLensFiles).
var LensFiles = []LensFile{
	{LensConfigFile, false},
	{LensExtractFile, true},
	{LensReviewFile, false},
}

// The lens.json keys. internal/lens's LensConfig struct tags must spell these (struct tags
// cannot reference constants; a test ties them), and the CLI's field writes use them.
const (
	LensKeyName         = "name" // no longer a setting: only a leftover key, read by lensJSONName
	LensKeyDimensions   = "dimensions"
	LensKeyRunner       = "runner"
	LensKeyExtractModel = "extract_model"
	LensKeyReviewModel  = "review_model"
)

// LensKeys are the lens.json keys witness knows, so doctor flags any other. A test proves they
// match LensConfig's tags, all but the ignored `name`.
var LensKeys = []string{LensKeyName, LensKeyDimensions, LensKeyRunner, LensKeyExtractModel, LensKeyReviewModel}

// ReadLensExtract reads a lens directory's mining prompt — the one required file — and
// refuses a missing or blank one: a lens without a mining prompt would mine nothing, and an
// empty prompt must be a loud error, never a silently-empty section.
func ReadLensExtract(dir string) ([]byte, error) {
	b, err := os.ReadFile(filepath.Join(dir, LensExtractFile))
	if err != nil {
		return nil, fmt.Errorf("missing %s (the mining prompt): %w", LensExtractFile, err)
	}
	if strings.TrimSpace(string(b)) == "" {
		return nil, fmt.Errorf("%s is empty (the mining prompt is required)", LensExtractFile)
	}
	return b, nil
}

// lensJSONName is the trimmed `name` left in dir's lens.json, or "" when there is none, it is
// blank, or the file is missing, unreadable or unparseable (the loader reports a broken file).
func lensJSONName(dir string) string {
	data, err := os.ReadFile(filepath.Join(dir, LensConfigFile))
	if err != nil {
		return ""
	}
	var cfg struct {
		Name string `json:"name"` // LensKeyName
	}
	if json.Unmarshal(data, &cfg) != nil {
		return ""
	}
	return strings.TrimSpace(cfg.Name)
}

// UnknownLensKeys returns the lens.json keys witness does not read, sorted. A missing or
// unparseable file returns nil. Case-insensitive, as the loader is: encoding/json matches a key
// to a struct field ignoring case, so "Extract_Model" is read and must not be called ignored.
func (r *lensReg) UnknownLensKeys(name string) []string {
	dir := filepath.Join(r.LensesDir(), sanitize(name))
	data, err := os.ReadFile(filepath.Join(dir, LensConfigFile))
	if err != nil {
		return nil
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(data, &m); err != nil {
		return nil
	}
	var unknown []string
	for k := range m {
		if !slices.ContainsFunc(LensKeys, func(known string) bool { return strings.EqualFold(k, known) }) {
			unknown = append(unknown, k)
		}
	}
	slices.Sort(unknown)
	return unknown
}

// ValidLensName is the lens-NAME rule, shared by RegisterLens and by `witness merge`, which
// brings lens names in from another archive's data and must refuse the same names registration
// would. A lens name ends up as a profile file name and a meta-key suffix, so an unchecked one
// is not cosmetic: "unified" collides with the cross-lens portrait's signature key, and a name
// with a "/" fails WriteProfile after the model call — so no signature is stamped and every
// profile read pays for a regeneration, forever.
func ValidLensName(name string) error {
	if ReservedLensName(name) {
		return fmt.Errorf("lens name %q is reserved (the cross-lens 'unified' summary); choose another name", name)
	}
	// Reject a name that isn't already a slug. The registry dir is sanitize(name)
	// (non-[A-Za-z0-9_-] → '_'), but every CLI gate (set/enable/backfill/show) and
	// LoadRegistered look the lens up by the RAW typed name — so a name like "my lens"
	// would be stored as "my_lens" yet be unaddressable under the name the tool accepted
	// and echoed. Requiring name == sanitize(name) keeps the stored name identical to the
	// handle, closing that gap at the single source instead of sanitizing at every gate.
	if sanitize(name) != name {
		return fmt.Errorf("lens name %q must be a slug — letters, digits, '-', '_' only (no spaces or special characters)", name)
	}
	return nil
}

// CheckLensRegistration runs every check RegisterLens makes BEFORE it touches the registry, and
// changes nothing. `witness merge` uses it so its dry run tells the truth: a lens it says "will
// be registered" must not then be refused by the apply — that disagreement is what kept a re-run
// from ever converging, re-taking a full-archive backup each time. Not under the registry lock: it
// is advisory, and RegisterLens re-checks under the lock before writing.
func (r *lensReg) CheckLensRegistration(name, srcDir string) error {
	if err := ValidLensName(name); err != nil {
		return err
	}
	// Reject a name that case-insensitively collides with an ALREADY-registered lens
	// under a different casing. The registry dir is sanitize(name) with case PRESERVED,
	// but witness's primary filesystems (macOS APFS, Windows NTFS) are case-insensitive,
	// so `register Default` would reuse `lenses/default`'s dir and clobber it. Pre-1a the
	// reserved-name gate blocked "Default" (case-folded); now that "default" is an
	// ordinary registerable lens that gate is gone, so guard the collision generally
	// (this protects ANY lens, not just default) — an exact-case re-register (name ==
	// existing) is still allowed (that's an intentional overwrite/update).
	lname := strings.ToLower(name)
	for _, existing := range r.RegisteredLenses() {
		if existing != name && strings.ToLower(existing) == lname {
			return fmt.Errorf("lens name %q collides with the already-registered %q on a case-insensitive filesystem; pick a distinct name or re-register %q exactly", name, existing, existing)
		}
	}

	info, err := os.Stat(srcDir)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("lens source %q must be a directory holding %s + %s (+ optional %s); the single-file lens format was replaced (issue #75)", srcDir, LensExtractFile, LensReviewFile, LensConfigFile)
	}
	if _, err := ReadLensExtract(srcDir); err != nil {
		return fmt.Errorf("lens source: %w", err)
	}
	return nil
}

// RegisterLens copies a lens definition DIRECTORY into the registry under `name`,
// creating/overwriting <root>/lenses/<name>/ with the source's LensFiles: extract.md
// (required — the mining prompt) and the optional lens.json and review.md. srcDir is the user's
// authored directory (issue #75: a lens is a directory, not one parsed file); only the LensFiles
// are copied, so stray files in the source dir are ignored.
//
// It is lossless under SELF-REGISTER (srcDir == the registry dir, i.e. the user edited
// the registered copy in place and re-registered it): ALL source files are read into
// memory BEFORE anything is removed, so the wipe can't delete a not-yet-read source
// file. And it stages into a sibling .tmp dir then atomically renames into place, so a
// concurrent worker read never sees a half-built lens directory.
func (r *lensReg) RegisterLens(name, srcDir string) error {
	// Serialize registry mutations (this + SetLensModel) so two concurrent
	// `witness lens register <same-name>` can't interleave through the shared staging
	// path and silently destroy the lens. Non-blocking: contention returns a retryable
	// error rather than corrupting — acceptable for a rare interactive admin op.
	unlock, ok := r.lensRegistryLock()
	if !ok {
		return errLensBusy
	}
	defer unlock()
	if err := r.CheckLensRegistration(name, srcDir); err != nil {
		return err
	}
	// Read EVERY source file into memory up front — before any destination mutation — so
	// a self-register (srcDir == dest) can't lose review.md/lens.json to the wipe below.
	files := map[string][]byte{}
	for _, f := range LensFiles {
		data, rerr := os.ReadFile(filepath.Join(srcDir, f.Name))
		switch {
		case rerr == nil:
			files[f.Name] = data
		case f.Required || !os.IsNotExist(rerr):
			return fmt.Errorf("read lens source %s: %w", f.Name, rerr)
		}
	}
	// Stage into a sibling .tmp dir, fully build it, then swap. A reader sees either the
	// old dir or the new one, never a half-built one.
	dir := filepath.Join(r.LensesDir(), sanitize(name))
	tmp := dir + ".tmp"
	bak := dir + ".bak"
	if err := os.RemoveAll(tmp); err != nil {
		return err
	}
	if err := os.MkdirAll(tmp, 0o700); err != nil {
		return err
	}
	for fn, data := range files {
		if err := os.WriteFile(filepath.Join(tmp, fn), data, 0o600); err != nil {
			_ = os.RemoveAll(tmp)
			return err
		}
	}
	// Swap with the OLD definition moved aside (not deleted) so a swap fault can't leave the
	// user with nothing — see swapDir. On failure the staged copy is KEPT for manual recovery
	// (the next register clears it), and the error says where the previous definition is.
	_ = os.RemoveAll(bak)
	if err := swapDir(tmp, dir, bak); err != nil {
		return fmt.Errorf("register lens %q: %w; new definition staged at %s", name, err, tmp)
	}
	return nil
}

// DeregisterLens removes a lens definition from the registry (no-op if absent).
// (It does not touch config; disable the lens separately if it was enabled.)
func (r *lensReg) DeregisterLens(name string) error {
	return os.RemoveAll(filepath.Join(r.LensesDir(), sanitize(name)))
}

// RegisteredLenses lists the names of lenses in the registry (dirs holding an
// extract.md — the one required file, so the presence probe never misses a lens that
// simply has no lens.json or review.md).
func (r *lensReg) RegisteredLenses() []string {
	entries, err := os.ReadDir(r.LensesDir())
	if err != nil {
		return nil
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() || isLensStagingDir(e.Name()) {
			continue
		}
		if _, err := os.Stat(filepath.Join(r.LensesDir(), e.Name(), LensExtractFile)); err == nil {
			names = append(names, e.Name())
		}
	}
	return names
}

// OldFormatLenses lists the registry dirs holding a pre-v0.3.0 single-file lens.md and no
// extract.md (issue #75). RegisteredLenses can't see them, so on their own they read as absent,
// or, if enabled, as an orphan whose hint said "disable". The converter that once rewrote them
// at every Open is gone (F065: every archive that matters is past v0.3.0); this is only so
// `lens list` and `doctor` can name the manual fix.
func (r *lensReg) OldFormatLenses() []string {
	entries, err := os.ReadDir(r.LensesDir())
	if err != nil {
		return nil
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() || isLensStagingDir(e.Name()) {
			continue
		}
		dir := filepath.Join(r.LensesDir(), e.Name())
		if _, err := os.Stat(filepath.Join(dir, LensExtractFile)); !os.IsNotExist(err) {
			continue
		}
		if _, err := os.Stat(filepath.Join(dir, "lens.md")); err == nil {
			names = append(names, e.Name())
		}
	}
	return names
}

// isLensStagingDir reports whether a registry entry is one of RegisterLens's transient
// staging/backup dirs (<name>.tmp / <name>.bak) rather than a real lens. A crash mid-swap
// can leave one behind; since a real lens name is a slug (RegisterLens rejects dots), no
// legitimate lens dir ends in these suffixes, so skipping them can never hide a real lens
// — it just keeps a crash artifact out of listings.
func isLensStagingDir(name string) bool {
	return strings.HasSuffix(name, ".tmp") || strings.HasSuffix(name, ".bak")
}

// IgnoredLensNames maps each registered lens whose lens.json still carries a `name` other than
// its registered name to that name. It is the one reader of the leftover key: the rename at Open
// acts on it, and `lens register` and doctor say it is ignored.
func (r *lensReg) IgnoredLensNames() map[string]string {
	ignored := map[string]string{}
	for _, reg := range r.RegisteredLenses() {
		if data := lensJSONName(filepath.Join(r.LensesDir(), reg)); data != "" && data != reg {
			ignored[reg] = data
		}
	}
	return ignored
}

// SetLensModel updates a registered lens's per-lens model in its lens.json (issue #75),
// creating the file if absent. phase selects the field: "extract" → extract_model,
// "review" → review_model. An empty value CLEARS the field (the lens then rides the
// default stage model). This is the safe struct round-trip that replaced hand-editing
// header directives: read → set one field → marshal → atomic write, so no text surgery
// can corrupt the file. It does NOT touch extract.md/review.md.
func (r *lensReg) SetLensModel(name, phase, model string) error {
	var field string
	switch phase {
	case "extract":
		field = LensKeyExtractModel
	case "review":
		field = LensKeyReviewModel
	default:
		return fmt.Errorf("unknown lens model phase %q (want extract|review)", phase)
	}
	return r.setLensJSONField(name, field, model)
}

// SetLensRunner sets a registered lens's per-lens runtime in its lens.json (issue #75
// slice 2): "claude"/"opencode" routes the lens's mine+review to that runner; an empty
// value CLEARS it so the lens rides the default runner. Same safe struct round-trip as
// SetLensModel — no text surgery. It does NOT validate the runner name here (an unknown
// name surfaces at drain time via the runner-set's circuit breaker + at `witness doctor`),
// matching how per-lens models are handled.
func (r *lensReg) SetLensRunner(name, runner string) error {
	return r.setLensJSONField(name, LensKeyRunner, runner)
}

// setLensJSONField is the shared, locked read-modify-write for a single lens.json field:
// preserve every other field, set the given one (or DELETE it when value is empty so the
// lens falls back to the default), and atomically write. This is the one place lens.json is
// mutated by the CLI — a struct/map round-trip, never text surgery (the #71 bug class).
func (r *lensReg) setLensJSONField(name, field, value string) error {
	// Same registry lock as RegisterLens: a lens.json write must not race a concurrent
	// register that is mid-swap on this lens's dir (which would read/write a lens.json
	// that's being renamed out from under it).
	unlock, ok := r.lensRegistryLock()
	if !ok {
		return errLensBusy
	}
	defer unlock()
	if !slices.Contains(r.RegisteredLenses(), name) {
		return fmt.Errorf("lens %q is not registered (run: witness lens register %s <dir>)", name, name)
	}
	path := filepath.Join(r.LensesDir(), sanitize(name), LensConfigFile)
	// Read-modify-write the existing lens.json (preserving other fields); an absent file
	// starts from an empty config.
	var raw map[string]any
	if data, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(data, &raw); err != nil {
			return fmt.Errorf("parse %s: %w", LensConfigFile, err)
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	if raw == nil {
		raw = map[string]any{}
	}
	if strings.TrimSpace(value) == "" {
		delete(raw, field) // clear → fall back to the default
	} else {
		raw[field] = strings.TrimSpace(value)
	}
	out, err := json.MarshalIndent(raw, "", "  ")
	if err != nil {
		return err
	}
	out = append(out, '\n')
	return writeAtomic(path, out)
}
