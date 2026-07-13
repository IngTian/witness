package commands

import (
	"log/slog"

	"github.com/IngTian/witness/internal/embed"
	"github.com/IngTian/witness/internal/store"
)

// maybeSpawnAutoWorker is the only path hooks/plugins use to start model work.
// Capture stays cheap and immediate; this just decides whether to kick a detached
// worker. There is deliberately NO debounce/cooldown: the machine-wide WorkerLock
// already single-flights the worker (a second spawn no-ops in milliseconds), and a
// running worker drains ALL pending work itself via its post-drain re-check loop —
// so throttling WHEN workers start bought nothing but the 1 Hz wakeup cascade it
// used to need. A ready embedding model is required for mining, but not for
// review-only work.
func maybeSpawnAutoWorker(st *store.Store) bool {
	cfg := st.LoadConfig()
	pending, _ := st.PendingSessions()
	modelReady := embed.ModelReady()
	if !autoWorkerShouldStart(st, cfg, pending, modelReady) {
		if len(pending) > 0 && !modelReady {
			slog.Info("distill: auto-start skipped; embedding model is not ready", "dir", embed.AssetsDir())
		}
		return false
	}
	_ = st.SetMetaString("worker_stop_requested", "")
	_ = st.SetMetaString("worker_mode", "auto-pending")
	spawnDetached("worker", "--auto")
	return true
}

// autoWorkerShouldStart decides whether an automatic worker should be kicked now.
// A worker already running needs no second spawn (it self-drains new arrivals), and
// mining without a ready model can't proceed (review-only work still can).
func autoWorkerShouldStart(st *store.Store, cfg store.Config, pending []string, modelReady bool) bool {
	if !cfg.AutoDistill {
		return false
	}
	hasPending := len(pending) > 0
	if !hasPending && !st.ReviewDue(cfg) {
		return false
	}
	if workerRunning(st) {
		return false
	}
	if hasPending && !modelReady {
		return false
	}
	return true
}

func workerRunning(st *store.Store) bool {
	status := st.MetaString("worker_status")
	return (status == "running" || status == "stopping") && workerPIDAlive(st.MetaString("worker_pid"))
}
