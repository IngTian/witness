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
// a registered lens. Two names are reserved (both defined in types.go, the single
// source of truth):
//   - LensDefault ("default") — the always-on built-in lens's identity. A second
//     lens under this name would share the built-in's (session,'default') watermark
//     and observation key, corrupting the backbone lens's data (two prompts writing
//     Lens='default', one progress row, cross-contaminated dedup).
//   - ProfileUnified ("unified") — the cross-lens profile summary's filename stem; a
//     per-lens summary under this name would clobber the unified portrait.
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
// issue #75) plus their one-shot legacy-format migration (lensmigrate.go). A
// filesystem leaf — it holds only the data root and never touches the DB. Its
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

// lensFileNames are the on-disk files of a lens directory. Duplicated from
// internal/lens (which the store must not import — store is the bottom of the stack)
// as small string literals; keep them in sync. lensConfigFile is the presence probe
// for RegisteredLenses.
const (
	lensConfigFile  = "lens.json"
	lensExtractFile = "extract.md"
	lensReviewFile  = "review.md"
)

// RegisterLens copies a lens definition DIRECTORY into the registry under `name`,
// creating/overwriting <root>/lenses/<name>/ with the source's lens.json (optional),
// extract.md (required — the mining prompt), and review.md (optional). srcDir is the
// user's authored directory (issue #75: a lens is a directory, not one parsed file);
// only the three known files are copied, so stray files in the source dir are ignored.
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
	if ReservedLensName(name) {
		return fmt.Errorf("lens name %q is reserved (the always-on built-in lens or the cross-lens summary); choose another name", name)
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
	info, err := os.Stat(srcDir)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("lens source %q must be a directory holding %s + %s (+ optional %s); the single-file lens format was replaced (issue #75)", srcDir, lensExtractFile, lensReviewFile, lensConfigFile)
	}
	// Read EVERY source file into memory up front — before any destination mutation — so
	// a self-register (srcDir == dest) can't lose review.md/lens.json to the wipe below.
	extract, err := os.ReadFile(filepath.Join(srcDir, lensExtractFile))
	if err != nil {
		return fmt.Errorf("lens source is missing %s (the mining prompt): %w", lensExtractFile, err)
	}
	if strings.TrimSpace(string(extract)) == "" {
		return fmt.Errorf("lens source %s is empty (the mining prompt is required)", lensExtractFile)
	}
	files := map[string][]byte{lensExtractFile: extract}
	for _, fn := range []string{lensReviewFile, lensConfigFile} { // both optional
		if data, rerr := os.ReadFile(filepath.Join(srcDir, fn)); rerr == nil {
			files[fn] = data
		} else if !os.IsNotExist(rerr) {
			return fmt.Errorf("read %s: %w", fn, rerr)
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
	// Move the OLD definition aside (not delete) so a swap fault can't leave the user with
	// nothing: if the Rename below fails, we restore it. Only after the new dir is in place
	// do we drop the backup. (A pre-swap failure here leaves the old lens untouched.)
	_ = os.RemoveAll(bak)
	hadOld := false
	if _, statErr := os.Stat(dir); statErr == nil {
		if err := os.Rename(dir, bak); err != nil {
			_ = os.RemoveAll(tmp)
			return err
		}
		hadOld = true
	}
	if err := os.Rename(tmp, dir); err != nil {
		// Swap failed: restore the previous definition and keep the staged copy for manual
		// recovery, with a self-explanatory error (never silently leave the lens gone).
		if hadOld {
			_ = os.Rename(bak, dir)
		}
		return fmt.Errorf("register lens %q failed during swap; previous definition %s, new definition staged at %s: %w",
			name, map[bool]string{true: "restored", false: "was absent"}[hadOld], tmp, err)
	}
	_ = os.RemoveAll(bak)
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
		if _, err := os.Stat(filepath.Join(r.LensesDir(), e.Name(), lensExtractFile)); err == nil {
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
		field = "extract_model"
	case "review":
		field = "review_model"
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
	return r.setLensJSONField(name, "runner", runner)
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
	path := filepath.Join(r.LensesDir(), sanitize(name), lensConfigFile)
	// Read-modify-write the existing lens.json (preserving other fields); an absent file
	// starts from an empty config.
	var raw map[string]any
	if data, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(data, &raw); err != nil {
			return fmt.Errorf("parse %s: %w", lensConfigFile, err)
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
