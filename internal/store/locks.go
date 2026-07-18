package store

import (
	"os"
	"path/filepath"
)

// procLocks is the cross-process advisory-lock concern: filesystem flocks under
// <root>/ that elect the single live worker and serialize per-source imports. A
// filesystem leaf (holds only root, never the DB), kept independent of the DB so
// leader election works the same regardless of storage backend. flockPath is a free
// function (not a method) because the lens registry also needs it without depending
// on procLocks — keeping both concerns leaves.
type procLocks struct{ root string }

// flockPath takes the OS-specific exclusive, non-blocking lock on <root>/<name>,
// creating the file if absent. Returns an unlock func and whether the lock was
// acquired. The shared primitive behind every witness lock (worker, import, lens
// registry). flockFile owns closing the descriptor — see flock_unix.go /
// flock_windows.go for the GOOS-split lock/unlock.
func flockPath(root, name string) (unlock func(), ok bool) {
	path := filepath.Join(root, name)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return func() {}, false
	}
	return flockFile(f)
}

// workerLockName is the single filesystem-flock file that elects the one live
// distillation worker. WorkerLock (the worker takes it for its whole run) and
// WorkerActive (a spawn/stop probe) share this one name so "the worker lock" has
// exactly one identity.
const workerLockName = ".worker.lock"

// WorkerLock serializes worker runs (leader election, not L0/L2 mutual exclusion).
// Returns an unlock func and whether the lock was acquired; if not, another
// worker is already draining the queue. Kept as a filesystem flock (independent
// of the DB) so it works the same regardless of storage backend.
func (p *procLocks) WorkerLock() (unlock func(), ok bool) {
	return flockPath(p.root, workerLockName)
}

// WorkerActive reports whether a distillation worker is currently live. It is the
// SOLE authority for that fact (issue #75 / #73-C2): the OS releases an flock when
// its holder dies — normal exit, SIGKILL, OOM, or power loss alike — so a failed
// acquisition means a genuinely-live holder, with no stale state to reconcile and no
// process probe. This replaces the old (worker_status meta + pid + `ps`) tri-source
// reconciliation, whose only job was to detect the crash case the OS already handles.
//
// Mechanism: try the lock non-blocking and, on success, immediately release it —
// acquisition proves no one else holds it. This is ADVISORY (for spawn/stop
// decisions), never the mutual-exclusion gate: the definitive single-flight is each
// worker holding WorkerLock for its whole run, so a redundantly-spawned worker just
// no-ops. The benign TOCTOU (state can change between this probe and a later spawn)
// therefore costs at most one detached process that exits immediately, never two
// concurrent drains. Do NOT call from inside the worker process that holds the lock:
// flockPath opens a fresh descriptor, which flock denies against the process's own
// held lock, so it would report "active" against itself.
//
// Two benign edge cases, both deliberately un-guarded (guarding them would add branch
// logic for outcomes that don't matter):
//   - Self-hold race: this briefly HOLDS the lock between acquire and release, so a
//     genuine worker's WorkerLock() can spuriously lose that microsecond race and
//     no-op. Harmless — the same probe then returns false and the spawn path re-spawns
//     (autoworker), while stop/status defer to the next trigger; the on-disk queue +
//     delta watermark guarantee no lost or doubled work.
//   - Open-failure conflation: flockPath returns ok=false BOTH for a held lock and for
//     an os.OpenFile failure (unwritable data dir), so a broken FS reads as "active".
//     Near-unreachable — store.Open() fails at the WAL pragma first, and .worker.lock
//     persists once created — and inert: WorkerLock is the same file, so a worker
//     no-ops there anyway. Worst case is a misleading status string in an already-
//     broken archive, never a functional regression.
func (p *procLocks) WorkerActive() bool {
	unlock, ok := flockPath(p.root, workerLockName)
	if ok {
		unlock()
		return false
	}
	return true
}

// ImportLock serializes a platform's import from its external source. The importer
// is watermark-based, but concurrent importers can otherwise read the same count
// and append the same text rows twice. name identifies the source (the platform
// owns it, so the store stays platform-agnostic) and keys a per-source lock file.
func (p *procLocks) ImportLock(name string) (unlock func(), ok bool) {
	return flockPath(p.root, "."+name+"-sync.lock")
}
