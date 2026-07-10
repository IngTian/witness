package commands

import (
	"log/slog"
	"time"

	"github.com/IngTian/witness/internal/embed"
	"github.com/IngTian/witness/internal/store"
)

const workerAutoStartedAtKey = "worker_auto_started_at"

type autoWorkerAction struct {
	start  bool
	wakeAt time.Time
}

// maybeSpawnAutoWorker is the only path hooks/plugins use to start model work.
// Capture stays cheap and immediate; this gate keeps automatic distillation
// laptop-friendly by requiring a non-running worker and an elapsed cooldown. A
// ready embedding model is required for mining, but not for review-only work.
func maybeSpawnAutoWorker(st *store.Store) bool {
	cfg := st.LoadConfig()
	pending, _ := st.PendingSessions()
	modelReady := embed.ModelReady()
	action := autoWorkerStartAction(st, cfg, pending, modelReady, time.Now())
	if !action.wakeAt.IsZero() {
		scheduleWorkerWakeup(st, action.wakeAt, "auto")
		return false
	}
	if !action.start {
		if len(pending) > 0 && !modelReady {
			slog.Info("distill: auto-start skipped; embedding model is not ready", "dir", embed.AssetsDir())
		}
		return false
	}
	_ = st.SetMetaString("worker_stop_requested", "")
	_ = st.SetMetaString("worker_mode", "auto-pending")
	_ = st.SetMetaString(workerAutoStartedAtKey, time.Now().UTC().Format(time.RFC3339))
	spawnDetached("worker", "--auto")
	return true
}

func autoWorkerStartAction(st *store.Store, cfg store.Config, pending []string, modelReady bool, now time.Time) autoWorkerAction {
	if !cfg.AutoDistill {
		return autoWorkerAction{}
	}
	hasPending := len(pending) > 0
	if !hasPending && !st.ReviewDue(cfg) {
		return autoWorkerAction{}
	}
	if workerRunning(st) {
		if next, ok := autoDistillNextAt(st, cfg.AutoDistillIntervalMinutes, now); ok {
			return autoWorkerAction{wakeAt: next}
		}
		return autoWorkerAction{wakeAt: now.Add(time.Second)}
	}
	if next, ok := autoDistillNextAt(st, cfg.AutoDistillIntervalMinutes, now); ok {
		return autoWorkerAction{wakeAt: next}
	}
	if hasPending && !modelReady {
		return autoWorkerAction{}
	}
	return autoWorkerAction{start: true}
}

func autoDistillNextAt(st *store.Store, intervalMinutes int, now time.Time) (time.Time, bool) {
	if intervalMinutes <= 0 {
		return time.Time{}, false
	}
	last := st.MetaString(workerAutoStartedAtKey)
	if last == "" {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339, last)
	if err != nil {
		return time.Time{}, false
	}
	next := t.Add(time.Duration(intervalMinutes) * time.Minute)
	if !now.Before(next) {
		return time.Time{}, false
	}
	return next, true
}

func workerRunning(st *store.Store) bool {
	status := st.MetaString("worker_status")
	return (status == "running" || status == "stopping") && workerPIDAlive(st.MetaString("worker_pid"))
}
