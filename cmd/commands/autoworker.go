package commands

import (
	"log/slog"
	"time"

	"github.com/IngTian/witness/internal/embed"
	"github.com/IngTian/witness/internal/store"
)

const workerAutoStartedAtKey = "worker_auto_started_at"

// maybeSpawnAutoWorker is the only path hooks/plugins use to start model work.
// Capture stays cheap and immediate; this gate keeps automatic distillation
// laptop-friendly by requiring a ready model, a non-running worker, pending work,
// and the configured cooldown to have elapsed.
func maybeSpawnAutoWorker(st *store.Store) bool {
	cfg := st.LoadConfig()
	if !cfg.AutoDistill {
		return false
	}
	pending, _ := st.PendingSessions()
	if len(pending) == 0 && !st.ReviewDue(cfg) {
		return false
	}
	if !embed.ModelReady() {
		slog.Info("distill: auto-start skipped; embedding model is not ready", "dir", embed.AssetsDir())
		return false
	}
	if workerRunning(st) {
		return false
	}
	if !autoDistillCooldownElapsed(st, cfg.AutoDistillIntervalMinutes) {
		return false
	}
	_ = st.SetMetaString(workerAutoStartedAtKey, time.Now().UTC().Format(time.RFC3339))
	spawnDetached("worker", "--auto")
	return true
}

func autoDistillCooldownElapsed(st *store.Store, intervalMinutes int) bool {
	if intervalMinutes <= 0 {
		return true
	}
	last := st.MetaString(workerAutoStartedAtKey)
	if last == "" {
		return true
	}
	t, err := time.Parse(time.RFC3339, last)
	if err != nil {
		return true
	}
	return time.Since(t) >= time.Duration(intervalMinutes)*time.Minute
}

func workerRunning(st *store.Store) bool {
	status := st.MetaString("worker_status")
	return (status == "running" || status == "stopping") && workerPIDAlive(st.MetaString("worker_pid"))
}
