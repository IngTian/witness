package commands

import (
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"github.com/IngTian/witness/internal/store"
	"github.com/spf13/cobra"
)

func newInternalWorkerWakeupCmd() *cobra.Command {
	return &cobra.Command{
		Use:    "worker-wakeup <seconds> [stamp] [mode]",
		Hidden: true,
		Args:   cobra.RangeArgs(1, 3),
		RunE: func(_ *cobra.Command, args []string) error {
			return cmdWorkerWakeup(args)
		},
	}
}

func cmdWorkerWakeup(args []string) error {
	seconds, err := strconv.Atoi(args[0])
	if err != nil || seconds < 0 {
		return fmt.Errorf("invalid wakeup delay %q", args[0])
	}
	if seconds > 0 {
		time.Sleep(time.Duration(seconds) * time.Second)
	}
	st, err := store.Open()
	if err != nil {
		return err
	}
	defer st.Close()
	cfg := st.LoadConfig()
	expectedStamp := ""
	if len(args) >= 2 {
		expectedStamp = args[1]
	}
	mode := ""
	if len(args) >= 3 {
		mode = args[2]
	}
	if mode == "" {
		if cfg.AutoDistill {
			mode = "auto"
		} else {
			mode = "manual"
		}
	}
	if expectedStamp != "" && st.MetaString(workerWakeupKey(mode)) != expectedStamp {
		return nil
	}
	if mode == "auto" && !cfg.AutoDistill {
		_ = clearScheduledWakeup(st, "auto")
		return nil
	}
	_ = clearScheduledWakeup(st, mode)
	// If runWorker reports !ran, another worker already holds the lock — and that
	// holder drains ALL pending work itself (its post-drain re-check loop keeps
	// going while `capture` lands new sessions mid-run), so there is nothing to
	// re-drive. We deliberately do NOT reschedule here: the old "lock held → wake
	// again in 1s" reschedule was a busy-poll that spawned ~1 detached process per
	// second for the running worker's whole life (the CPU peg). A genuinely deferred
	// need — a backed-off session due later, with no worker running — is covered by
	// scheduleRetryWakeup, which schedules that single real future wakeup on exit.
	_, err = runWorker(mode == "auto")
	return err
}

func scheduleRetryWakeup(st *store.Store) {
	next, ok := st.NextBackoffAttempt(activeLensNames(st), time.Now())
	if !ok {
		return
	}
	scheduleWorkerWakeup(st, next, workerWakeMode(st))
}

func scheduleWorkerWakeup(st *store.Store, next time.Time, mode string) {
	scheduleWorkerWakeupWith(st, next, mode, spawnDetachedOK)
}

// scheduleWorkerWakeupWith arms the single future wakeup that re-drives a backed-off
// session. `spawn` reports whether the child actually started.
//
// The stamp is written only AFTER a successful spawn. It used to be written first, and the
// spawn's error was discarded (spawnDetached returned nothing), so a failed spawn left a
// PHANTOM LATCH: the dedup guard below sees a future stamp and suppresses every subsequent
// attempt to arm the wakeup, including from other processes, for the whole remaining backoff
// window — up to the 6h cap. Reproduced: with a spawn that does nothing, the stamp is still
// written and the next two re-arm attempts spawn 0 children.
//
// No data is lost (raw L0 is durable and the watermark did not advance), but the automatic
// retry never fires, so the delta waits for an unrelated external trigger — the user's next
// session-start/-end, or the OpenCode plugin's quiet-period kick. On a machine that just hit
// a transient rate limit, that is exactly when the retry was supposed to be automatic.
//
// Ordering is safe the other way round: stamping after the spawn cannot cause a double-spawn,
// because the child re-reads the stamp on wake and exits if it does not match (see
// cmdWorkerWakeup's expectedStamp check). A brief window where a spawned-but-unstamped child
// exists is therefore self-correcting, whereas a stamped-but-unspawned latch is not.
func scheduleWorkerWakeupWith(st *store.Store, next time.Time, mode string, spawn func(...string) bool) {
	if mode != "auto" {
		mode = "manual"
	}
	stamp := next.UTC().Format(time.RFC3339Nano)
	key := workerWakeupKey(mode)
	if current, err := time.Parse(time.RFC3339Nano, st.MetaString(key)); err == nil && current.After(time.Now()) && !current.After(next) {
		return // an earlier wakeup already covers this work
	}
	delay := time.Until(next)
	if delay < 0 {
		delay = 0
	}
	seconds := int(delay/time.Second) + 1
	if !spawn("worker-wakeup", strconv.Itoa(seconds), stamp, mode) {
		// Leave the stamp untouched so the NEXT trigger re-attempts instead of trusting a
		// wakeup that does not exist. Loud, because the automatic retry is now not armed.
		slog.Error("distill: could not spawn the worker wakeup; the automatic retry is NOT armed "+
			"(a later trigger will re-attempt)", "at", stamp, "mode", mode)
		return
	}
	_ = st.SetMetaString(key, stamp)
	slog.Info("distill: scheduled worker wakeup", "at", stamp, "delay", delay.String(), "mode", mode)
}

func clearScheduledWakeup(st *store.Store, mode string) bool {
	if mode == "" {
		clearedAuto := clearScheduledWakeup(st, "auto")
		return clearScheduledWakeup(st, "manual") || clearedAuto
	}
	key := workerWakeupKey(mode)
	if st.MetaString(key) == "" {
		return false
	}
	_ = st.SetMetaString(key, "")
	return true
}

func workerWakeMode(st *store.Store) string {
	if mode := st.MetaString("worker_mode"); mode != "" {
		return mode
	}
	return "manual"
}

func workerWakeupKey(mode string) string {
	if mode == "auto" {
		return "worker_auto_wakeup_at"
	}
	return "worker_manual_wakeup_at"
}
