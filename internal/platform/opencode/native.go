package opencode

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/IngTian/witness/internal/platform"
)

// nativeCommand is injectable so tests prove the two DB environments without a
// real OpenCode binary. Production uses CommandContext through this small port.
var nativeCommand = func(ctx context.Context, env []string, args ...string) ([]byte, error) {
	return commandOutput(ctx, env, args...)
}

type nativeRuntime struct {
	root   string
	server *OpenCodeServer
	mu     sync.Mutex
}
type nativeManifest struct {
	Session, Lens, Input, Digest, Fork, Reply, MessageID string
	RawHigh                                              int64
	Total                                                int
	Committed                                            bool
}

func newNativeRuntime(root string, server *OpenCodeServer) *nativeRuntime {
	return &nativeRuntime{root: root, server: server}
}
func (n *nativeRuntime) dir() string { return filepath.Join(n.root, "opencode-native") }

// path is the manifest key: the identity of one retained generation.
//
// It covers the REQUEST as well as the input, because run() short-circuits on a cached
// Reply without re-prompting. Keying on (session, rawHigh, lens, input) alone would make
// the manifest a second, invisible cache of derived state that survives `lens backfill
// --fresh` (which clears only DB state — DeleteLensData + ResetLensWatermark — and cannot
// see this directory): editing a lens prompt or switching triage_model and re-backfilling
// would replay the OLD model's answer into L1 as if it were fresh. Folding model+prompt
// into the key means a changed request simply misses the cache and re-generates, while an
// interrupted identical request still resumes.
func (n *nativeRuntime) path(w *platform.NativeSession, model, prompt string) string {
	h := sha256.Sum256([]byte(w.Session + "\x00" + fmt.Sprint(w.RawHigh) + "\x00" + w.Lens + "\x00" + w.Input +
		"\x00" + model + "\x00" + prompt))
	return filepath.Join(n.dir(), fmt.Sprintf("%x.json", h[:]))
}
func (n *nativeRuntime) snapshot(p string) string {
	return strings.TrimSuffix(p, ".json") + ".snapshot.json"
}

func isolatedEnv(root string) []string {
	return []string{"XDG_DATA_HOME=" + filepath.Join(root, "xdg"), "OPENCODE_DB=" + filepath.Join(root, "opencode.db")}
}
func replaceEnv(base, updates []string) []string {
	m := map[string]string{}
	order := []string{}
	for _, e := range base {
		k, _, _ := strings.Cut(e, "=")
		if _, ok := m[k]; !ok {
			order = append(order, k)
		}
		m[k] = e
	}
	for _, e := range updates {
		k, _, _ := strings.Cut(e, "=")
		if _, ok := m[k]; !ok {
			order = append(order, k)
		}
		m[k] = e
	}
	out := make([]string, 0, len(order))
	for _, k := range order {
		out = append(out, m[k])
	}
	return out
}

func (n *nativeRuntime) run(ctx context.Context, w *platform.NativeSession, model, prompt, input string) (string, error) {
	n.mu.Lock()
	locked := true
	defer func() {
		if locked {
			n.mu.Unlock()
		}
	}()
	if err := os.MkdirAll(n.dir(), 0o700); err != nil {
		return "", err
	}
	p := n.path(w, model, prompt)
	m, err := n.load(p)
	if err != nil {
		return "", err
	}
	if m.Session == "" {
		m = nativeManifest{Session: w.Session, RawHigh: w.RawHigh, Total: w.Total, Lens: w.Lens, Input: w.Input, Digest: w.Digest}
	} else if m.Digest != w.Digest {
		return "", fmt.Errorf("opencode native retained manifest digest does not match L0 generation")
	}
	if m.Reply != "" {
		w.SetFinalizer(func() error { return n.finalize(p) })
		return m.Reply, nil
	}
	if m.Fork == "" {
		snap := n.snapshot(p)
		if _, err := os.Stat(snap); os.IsNotExist(err) {
			if err = n.export(ctx, w, snap); err != nil {
				return "", err
			}
		} else if err != nil {
			return "", err
		} else if w.Digest != "" {
			// A RETAINED snapshot is reused, but only if it is still readable AND matches
			// this L0 generation. An unusable one is discarded and re-exported once rather
			// than returned as an error: a snapshot truncated by a crash or ENOSPC would
			// otherwise satisfy the existence check above forever, failing validation on
			// every retry with nothing able to replace it (a permanently wedged lens).
			// A digest mismatch on the FRESH export is still fatal — that means the user's
			// session genuinely changed after L0 capture, which must not be distilled.
			data, err := os.ReadFile(snap)
			if err != nil || validateExportDigest(data, w.Digest) != nil {
				if err = os.Remove(snap); err != nil && !os.IsNotExist(err) {
					return "", err
				}
				if err = n.export(ctx, w, snap); err != nil {
					return "", err
				}
			}
		}
		// A previous lens may have left its disposable pristine import behind.
		// This server is bound to the private DB, so deleting here can never touch
		// the user's source session.
		if err = n.server.deleteSession(ctx, strings.TrimPrefix(w.Session, SessionPrefix)); err != nil {
			return "", fmt.Errorf("clear isolated opencode source: %w", err)
		}
		if err = n.importSnapshot(ctx, snap); err != nil {
			return "", err
		}
		id, err := n.server.fork(ctx, strings.TrimPrefix(w.Session, SessionPrefix))
		if err != nil {
			return "", err
		}
		m.Fork = id
		// KNOWN LIMITATION (accepted, bounded): a kill in the two statements between fork()
		// returning and this save leaves a fork in the ISOLATED db that no manifest names,
		// so reconcile — which only knows m.Fork — can never reap it. The resume re-forks
		// (m.Fork is still ""), so nothing is lost or double-committed; the cost is one
		// stranded session per crash landing in that window, inside witness's own private
		// database. Reaping it would need a session-LIST endpoint plus "delete every fork no
		// manifest claims", which is more machinery (and more delete authority) than a
		// leak of this size justifies. Saving BEFORE the fork cannot help: the id does not
		// exist yet, so the manifest still would not name it.
		if err = n.save(p, m); err != nil {
			return "", err
		}
		n.server.deleteSessionBestEffort(strings.TrimPrefix(w.Session, SessionPrefix))
	}
	newRequest := m.MessageID == ""
	if newRequest {
		m.MessageID = "msg_" + mustRandomHex(12)
		if err = n.save(p, m); err != nil {
			return "", err
		}
	}
	// Import/fork setup is serialized; independent fork generation is not.
	n.mu.Unlock()
	locked = false
	if reply, err := n.server.replyForMessage(ctx, m.Fork, m.MessageID); err == nil && reply != "" {
		m.Reply = reply
	} else if m.Reply == "" {
		// A retained request with no completed assistant reply was interrupted.
		// Retry on the same native fork with a fresh message id rather than
		// colliding with OpenCode's unique message id constraint.
		if !newRequest {
			m.MessageID = "msg_" + mustRandomHex(12)
			n.mu.Lock()
			locked = true
			if err = n.save(p, m); err != nil {
				return "", err
			}
			n.mu.Unlock()
			locked = false
		}
		m.Reply, err = n.server.runSessionWithMessage(ctx, m.Fork, m.MessageID, model, prompt, input)
		if err != nil {
			return "", err
		}
	}
	n.mu.Lock()
	locked = true
	if err = n.save(p, m); err != nil {
		return "", err
	}
	n.mu.Unlock()
	locked = false
	w.SetFinalizer(func() error { return n.finalize(p) })
	return m.Reply, nil
}

func (n *nativeRuntime) load(p string) (nativeManifest, error) {
	b, err := os.ReadFile(p)
	if os.IsNotExist(err) {
		return nativeManifest{}, nil
	}
	if err != nil {
		return nativeManifest{}, err
	}
	var m nativeManifest
	if err = json.Unmarshal(b, &m); err != nil {
		return m, fmt.Errorf("opencode native retained manifest %s is malformed: %w", p, err)
	}
	if m.Session == "" || m.Lens == "" || m.Total < 0 {
		return m, fmt.Errorf("opencode native retained manifest %s is malformed", p)
	}
	return m, nil
}
func (n *nativeRuntime) save(p string, m nativeManifest) error {
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	tmp := p + ".tmp"
	if err = os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

// export writes the user's session transcript to snap as an `opencode export --pure`
// payload.
//
// The source session lives in the USER's opencode.db, but witness must never hand that
// path to a writable subprocess: `opencode export` opens its OPENCODE_DB read-WRITE and
// durably mutates it on every run (empirically, ~4KB of WAL per export, and it applies
// schema migrations), which would defeat this package's whole read-only-user-DB premise
// and leave witness able to corrupt the user's DB if killed mid-write. So we snapshot
// the user DB read-only first (VACUUM INTO through a mode=ro DSN — a consistent copy
// even while OpenCode is writing, and pure-Go) and point the subprocess at the COPY,
// with isolatedEnv so XDG_DATA_HOME can't lead it back to the user's data dir either.
// The copy is disposable: it is removed here, and reconcile sweeps any leftover.
func (n *nativeRuntime) export(ctx context.Context, w *platform.NativeSession, snap string) error {
	src, err := DefaultDBPath()
	if err != nil {
		return err
	}
	if err = os.MkdirAll(n.dir(), 0o700); err != nil {
		return err
	}
	// A per-call name so concurrent lenses/sessions can never share (or delete) one
	// another's copy; sweepable by reconcile via the sourceCopyPrefix.
	copyPath := filepath.Join(n.dir(), sourceCopyPrefix+mustRandomHex(8)+".db")
	if err = snapshotSourceDB(src, copyPath); err != nil {
		return err
	}
	defer func() {
		for _, suffix := range []string{"", "-wal", "-shm"} {
			_ = os.Remove(copyPath + suffix)
		}
	}()
	env := replaceEnv(os.Environ(), append(isolatedEnv(n.root), "OPENCODE_DB="+copyPath))
	b, err := nativeCommand(ctx, env, "export", "--pure", strings.TrimPrefix(w.Session, SessionPrefix))
	if err != nil {
		return fmt.Errorf("opencode native export unavailable; upgrade to OpenCode 1.18.0+: %w", err)
	}
	if w.Digest != "" {
		if err := validateExportDigest(b, w.Digest); err != nil {
			return err
		}
	}
	// Atomic, like save(): a partial snapshot (crash or ENOSPC mid-write) would pass the
	// existence check in run() and then fail digest validation forever, wedging the
	// generation with no path to re-export.
	tmp := snap + ".tmp"
	if err = os.WriteFile(tmp, b, 0o600); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, snap)
}

// sourceCopyPrefix marks the disposable read-only copies export() makes of the user's
// opencode.db, so reconcile can sweep one left behind by a crash.
const sourceCopyPrefix = "source-"

// snapshotSourceDB copies src to dst via VACUUM INTO over a READ-ONLY connection, so
// the user's database is never opened writable. VACUUM INTO reads a consistent view
// even while OpenCode is writing, and folds the WAL in, so dst needs no sidecars.
func snapshotSourceDB(src, dst string) error {
	if _, err := os.Stat(src); err != nil {
		return fmt.Errorf("opencode source db: %w", err)
	}
	for _, suffix := range []string{"", "-wal", "-shm"} {
		_ = os.Remove(dst + suffix) // VACUUM INTO requires a fresh destination
	}
	db, err := sql.Open("sqlite", readOnlyURI(src))
	if err != nil {
		return err
	}
	defer db.Close()
	if _, err = db.Exec("VACUUM INTO ?", dst); err != nil {
		return fmt.Errorf("snapshot opencode source db: %w", err)
	}
	return nil
}

func validateExportDigest(data []byte, want string) error {
	digest, err := exportedTranscriptDigest(data)
	if err != nil {
		return err
	}
	if digest != want {
		return fmt.Errorf("opencode native session changed after L0 capture; generation remains pending")
	}
	return nil
}

func exportedTranscriptDigest(data []byte) (string, error) {
	var exported struct {
		Messages []struct {
			Info struct {
				Role string `json:"role"`
				Time struct {
					Completed int64 `json:"completed"`
				} `json:"time"`
			} `json:"info"`
			Parts []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"parts"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(data, &exported); err != nil {
		return "", fmt.Errorf("decode opencode native export: %w", err)
	}
	entries := make([]platform.TranscriptEntry, 0, len(exported.Messages))
	for _, message := range exported.Messages {
		if message.Info.Role != "user" && message.Info.Role != "assistant" {
			continue
		}
		if message.Info.Role == "assistant" && message.Info.Time.Completed == 0 {
			continue
		}
		var text strings.Builder
		for _, part := range message.Parts {
			if part.Type != "text" || strings.TrimSpace(part.Text) == "" {
				continue
			}
			if text.Len() > 0 {
				text.WriteString("\n\n")
			}
			text.WriteString(part.Text)
		}
		if value := strings.TrimSpace(text.String()); value != "" {
			entries = append(entries, platform.TranscriptEntry{Role: message.Info.Role, Text: value})
		}
	}
	return platform.TranscriptDigest(entries), nil
}
func (n *nativeRuntime) importSnapshot(ctx context.Context, snap string) error {
	if err := n.prepareAuth(); err != nil {
		return err
	}
	_, err := nativeCommand(ctx, replaceEnv(os.Environ(), isolatedEnv(n.root)), "import", "--pure", snap)
	if err != nil {
		return fmt.Errorf("opencode native import: %w", err)
	}
	return nil
}

// prepareAuth copies, never links or writes, user auth. Missing auth is normal for
// environment-backed providers.
func (n *nativeRuntime) prepareAuth() error {
	db, err := DefaultDBPath()
	if err != nil {
		return err
	}
	src := filepath.Join(filepath.Dir(db), "auth.json")
	si, err := os.Stat(src)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	dst := filepath.Join(n.root, "xdg", "opencode", "auth.json")
	if li, err := os.Lstat(dst); err == nil && li.Mode()&os.ModeSymlink != 0 {
		if err := os.Remove(dst); err != nil {
			return err
		}
	}
	if di, err := os.Stat(dst); err == nil && !si.ModTime().After(di.ModTime()) {
		return os.Chmod(dst, 0o600)
	}
	b, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	if err = os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return err
	}
	return os.WriteFile(dst, b, 0o600)
}

func (n *nativeRuntime) finalize(p string) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	m, err := n.load(p)
	if err != nil {
		return err
	}
	m.Committed = true
	if err = n.save(p, m); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if m.Fork != "" {
		if err = n.server.deleteSession(ctx, m.Fork); err != nil {
			return err
		}
	}
	if err = os.Remove(n.snapshot(p)); err != nil && !os.IsNotExist(err) {
		return err
	}
	return os.Remove(p)
}

// reconcile closes the post-L1/pre-finalizer crash window. The raw high id is the
// store's generation token: a missing token (or one a later mine has already grown
// past) means a replace-import, cleanup, or newer generation superseded this manifest,
// so its private artifacts are no longer useful.
//
// Every entry is fault-ISOLATED: a problem with one manifest is logged and skipped,
// never returned. os.ReadDir sorts by name, so a single unreadable entry used to end
// the sweep and hide every lexicographically later manifest — and because runner Open
// treats a reconcile error as fatal, one corrupt file could block all OpenCode
// distillation. A manifest that cannot be parsed at all is reaped (its fork id is
// unrecoverable, so retaining it leaks forever) rather than retained.
//
// It also sweeps residue the manifest loop cannot see (orphan snapshots, .tmp files,
// and leftover source-db copies), which is otherwise unreachable: export() writes the
// snapshot BEFORE the manifest exists, so a crash in that window leaves a file nothing
// ever deletes. Safe to do here — reconcile runs from runner Open before any Run, so no
// generation is in flight.
func (n *nativeRuntime) reconcile() error {
	entries, err := os.ReadDir(n.dir())
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	manifests := map[string]bool{}
	for _, x := range entries {
		name := x.Name()
		if !strings.HasSuffix(name, ".json") || strings.HasSuffix(name, ".snapshot.json") {
			continue
		}
		manifests[strings.TrimSuffix(name, ".json")] = true
		p := filepath.Join(n.dir(), name)
		m, err := n.load(p)
		if err != nil {
			// Unparseable: the fork id is unrecoverable, so nothing can ever finalize it.
			// Drop the manifest + its snapshot so the directory converges.
			slog.Warn("opencode native: discarding unreadable manifest", "path", p, "err", err)
			_ = os.Remove(n.snapshot(p))
			_ = os.Remove(p)
			continue
		}
		if m.Committed {
			if err = n.finalize(p); err != nil {
				slog.Warn("opencode native: finalize retained for retry", "path", p, "err", err)
			}
			continue
		}
		current, committed, err := n.generationStatus(m)
		if err != nil {
			slog.Warn("opencode native: retaining generation without store evidence", "path", p, "err", err)
			continue // without store evidence, retain an uncommitted generation
		}
		if !current || committed {
			m.Committed = true
			if err = n.save(p, m); err != nil {
				slog.Warn("opencode native: could not mark manifest committed", "path", p, "err", err)
				continue
			}
			if err = n.finalize(p); err != nil {
				slog.Warn("opencode native: finalize retained for retry", "path", p, "err", err)
			}
		}
	}
	n.sweepResidue(entries, manifests)
	return nil
}

// sweepResidue removes files in the native dir that no live manifest owns: a snapshot
// whose manifest is gone (or was never written — export() writes the snapshot first),
// an interrupted save()/export() .tmp, and a disposable source-db copy left by a crash
// mid-export. Called at the end of reconcile, when nothing is in flight.
func (n *nativeRuntime) sweepResidue(entries []os.DirEntry, manifests map[string]bool) {
	for _, x := range entries {
		name := x.Name()
		switch {
		case strings.HasSuffix(name, ".snapshot.json"):
			if manifests[strings.TrimSuffix(name, ".snapshot.json")] {
				continue // still owned by a live manifest
			}
		case strings.HasSuffix(name, ".tmp"), strings.HasPrefix(name, sourceCopyPrefix):
			// Never live at Open: save/export rename into place, and a source copy is
			// deleted by the export that made it.
		default:
			continue
		}
		p := filepath.Join(n.dir(), name)
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
			slog.Warn("opencode native: could not remove residue", "path", p, "err", err)
			continue
		}
		slog.Debug("opencode native: swept residue", "path", p)
	}
}

// generationStatus reports whether a manifest's generation is still the one a commit
// could land for (current) and whether it already landed (committed), reading the
// witness DB READ-ONLY.
//
// "Current" requires the manifest's raw high id to still exist AND to still be the
// session's high-water mark. The `id > RawHigh` clause is what bounds accumulation: a
// mine always reads the session's present max(raw.id), so once a later turn arrives the
// older generation can never be the one that commits — without this, a lens that keeps
// failing (or one later disabled) strands one manifest + full-session snapshot + live
// isolated fork per drain, forever.
func (n *nativeRuntime) generationStatus(m nativeManifest) (current, committed bool, err error) {
	db, err := sql.Open("sqlite", readOnlyURI(filepath.Join(filepath.Dir(n.root), "witness.db")))
	if err != nil {
		return false, false, err
	}
	defer db.Close()
	var currentValue, committedValue int
	err = db.QueryRow(`SELECT
		CASE WHEN (? = 0 AND NOT EXISTS (SELECT 1 FROM raw WHERE session = ?))
		       OR (EXISTS (SELECT 1 FROM raw WHERE session = ? AND id = ?)
		           AND NOT EXISTS (SELECT 1 FROM raw WHERE session = ? AND id > ?)) THEN 1 ELSE 0 END,
		CASE WHEN COALESCE((SELECT distilled >= ? FROM progress WHERE session = ? AND lens = ?), 0)
		     THEN 1 ELSE 0 END`,
		m.RawHigh, m.Session, m.Session, m.RawHigh, m.Session, m.RawHigh, m.Total, m.Session, m.Lens,
	).Scan(&currentValue, &committedValue)
	if err != nil {
		return false, false, err
	}
	return currentValue != 0, committedValue != 0, nil
}

// readOnlyURI builds the SQLite `file:` URI witness uses to read a database WITHOUT being
// able to write it — the guarantee that keeps distillation off the user's own opencode.db.
//
// It must NOT be built with url.URL{Scheme:"file", Path: p}. On Windows that produced
// `file://C:%5CUsers%5C...`: the drive letter is parsed as the URI AUTHORITY and every
// backslash is percent-encoded, so SQLite rejects it outright. Reported from a real Windows
// run (issue #10):
//
//	witness.exe import --agent opencode
//	witness: SQL logic error: invalid uri authority:
//	  C:%5CUsers%5Ctzy20%5C.local%5Cshare%5Copencode%5Copencode.db?cache=shared&mode=ro (1)
//
// A SQLite file: URI wants FORWARD slashes and a leading `/` before the drive letter
// (`file:/C:/Users/...`), and the path must not be percent-escaped. So compose it directly
// rather than through url.URL, whose escaping rules are for HTTP-shaped URLs.
//
// Note filepath.ToSlash is NOT usable here: it is a no-op on non-Windows, so a Windows path
// handled on any other GOOS (or in a cross-platform test) would keep its backslashes. The
// replacement is unconditional on purpose, which also makes this testable off Windows.
func readOnlyURI(path string) string {
	return sqliteFileURI(path, "mode=ro")
}

// sqliteFileURI is the one place a SQLite file: URI is composed, so the Windows escaping rule
// above cannot be re-broken in one caller and not the other (it previously existed as two
// identical copies here and in import.go, and both were wrong the same way).
//
// query is appended verbatim (already-encoded k=v&k=v), because these are fixed internal
// values — no user input reaches it, so there is nothing to escape.
//
// The path needs SELECTIVE escaping, which is the subtlety that makes hand-rolling this
// dangerous in both directions:
//   - '?' and '#' MUST be escaped. They terminate the path (query / fragment), so a real data
//     dir like `witness#archive.db` would silently open a DIFFERENT, empty database — reading
//     the wrong file rather than failing. (A regression test covers exactly that path.)
//   - '%' MUST be escaped first, or an existing percent in a filename would be re-read as an
//     escape sequence.
//   - ':' and '/' must NOT be escaped: the drive colon in `C:/…` and the separators are
//     structural. This is precisely where url.URL got it wrong — it percent-encoded the
//     backslashes and promoted `C:` to an authority.
func sqliteFileURI(path, query string) string {
	// Only resolve a RELATIVE path. filepath.Abs is GOOS-dependent: on non-Windows it does not
	// recognize `C:\dir\file` as absolute and prepends the current directory, producing
	// `file:/<cwd>/C:/dir/file`. That matters for more than tests — it is why this rule must be
	// explicit rather than delegated — and it keeps the function verifiable off Windows.
	if !isAbsolutePath(path) {
		if abs, err := filepath.Abs(path); err == nil {
			path = abs
		}
	}
	p := strings.ReplaceAll(path, `\`, "/")
	// Order matters: '%' first, so the escapes introduced below are not double-escaped.
	p = strings.ReplaceAll(p, "%", "%25")
	p = strings.ReplaceAll(p, "?", "%3F")
	p = strings.ReplaceAll(p, "#", "%23")
	if !strings.HasPrefix(p, "/") {
		p = "/" + p // a Windows path starts at the drive letter: C:/... -> /C:/...
	}
	uri := "file:" + p
	if query != "" {
		uri += "?" + query
	}
	return uri
}

// isAbsolutePath recognizes an absolute path in EITHER convention, regardless of the running
// GOOS: a leading separator (`/foo`, `\\server\share`) or a drive letter (`C:\foo`, `c:/foo`).
// filepath.IsAbs only understands the host's own convention, which is not enough here — the
// same composition must behave identically wherever it runs.
func isAbsolutePath(p string) bool {
	if p == "" {
		return false
	}
	if p[0] == '/' || p[0] == '\\' {
		return true
	}
	// Drive-letter form: exactly one ASCII letter, a colon, then a separator.
	if len(p) >= 3 && p[1] == ':' && (p[2] == '\\' || p[2] == '/') {
		c := p[0] | 0x20 // lowercase
		return c >= 'a' && c <= 'z'
	}
	return false
}
func commandOutput(ctx context.Context, env []string, args ...string) ([]byte, error) {
	if len(args) == 0 {
		return nil, fmt.Errorf("missing opencode command")
	}
	binary, err := openCodeExe()
	if err != nil {
		binary = "opencode"
	}
	c := execCommandContext(ctx, binary, args...)
	c.Env = env
	// Pin the cwd, like the two sibling invocations in server.go. OpenCode derives the
	// PROJECT it records (worktree, vcs, sandboxes) from the working directory, so
	// inheriting the worker's cwd — which is a user repo, since neither spawnDetached nor
	// the plugin's Bun.spawn sets one — makes a witness subprocess rewrite project
	// bookkeeping for whatever repo the worker happened to start in. A neutral temp dir
	// keeps witness's own invocations from carrying project identity at all.
	c.Dir = os.TempDir()
	procCtl.BindToParent(c)
	var stdout, stderr bytes.Buffer
	c.Stdout = &stdout
	c.Stderr = &stderr
	if err := c.Run(); err != nil {
		return stdout.Bytes(), fmt.Errorf("%w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return stdout.Bytes(), nil
}

// indirection keeps command execution testable without touching process globals.
var execCommandContext = func(ctx context.Context, name string, args ...string) *exec.Cmd {
	return exec.CommandContext(ctx, name, args...)
}
