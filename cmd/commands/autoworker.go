package commands

import (
	"log/slog"

	"github.com/IngTian/witness/internal/embed"
	"github.com/IngTian/witness/internal/store"
	"github.com/spf13/cobra"
)

// newInternalWorkerKickCmd is the plugin-facing "maybe start an auto worker" entry
// point: it runs the SAME gate the capture hooks use (maybeSpawnAutoWorker), rather
// than spawning `worker-run --auto` directly.
//
// Why it exists: `worker stop --auto-only` (the OpenCode plugin's dispose) latches a
// DURABLE `worker_stop_requested` meta flag, and an auto worker refuses to run while
// it is set (cmdWorker) — only maybeSpawnAutoWorker and a MANUAL run clear it. A
// caller that spawns `worker-run --auto` itself therefore no-ops forever after the
// first dispose, silently freezing automatic distillation for an OpenCode-only user
// (a Claude Code user is rescued incidentally by their capture hook clearing it).
// Routing the plugin's quiet-period start through this command keeps the flag's
// clear-on-intentional-start contract in ONE place.
func newInternalWorkerKickCmd() *cobra.Command {
	return &cobra.Command{Use: "worker-kick", Hidden: true, Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			st, err := store.Open()
			if err != nil {
				return err
			}
			defer st.Close()
			defer setupLogging(st)()
			maybeSpawnAutoWorker(st)
			return nil
		}}
}

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
	// Clear the durable stop flag FIRST, before the should-start gate. `distill stop
	// --auto-only` (the OpenCode plugin's dispose) latches it, and an auto worker refuses
	// to run while it is set — so it must be released by the mere ARRIVAL of a fresh
	// auto-start intent, not by a successful spawn. Clearing it after the gate would leave
	// a user frozen whenever the gate declines for a transient reason (no pending work
	// yet, a worker already live, the embedding model still downloading): the flag would
	// survive to block every later start too.
	if st.MetaString("worker_stop_requested") == "1" {
		_ = st.SetMetaString("worker_stop_requested", "")
	}
	pending, _ := st.PendingSessions(activeLensNames(st))
	modelReady := embed.ModelReady()
	if !autoWorkerShouldStart(st, cfg, pending, modelReady) {
		if len(pending) > 0 && !modelReady {
			slog.Info("distill: auto-start skipped; embedding model is not ready", "dir", embed.AssetsDir())
		}
		return false
	}
	_ = st.SetMetaString("worker_mode", "auto-pending")
	spawnDetached("worker-run", "--auto")
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
	if st.WorkerActive() {
		return false
	}
	if hasPending && !modelReady {
		return false
	}
	return true
}
