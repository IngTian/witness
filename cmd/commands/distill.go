package commands

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/IngTian/witness/internal/store"
	"github.com/spf13/cobra"
)

func newDistillCmd() *cobra.Command {
	distillCmd := &cobra.Command{
		Use:   "distill",
		Short: "Manage the background distillation worker.",
		Long:  "Start, inspect, or stop the single-flight worker that turns raw turns into observations, reviews facets when due, and regenerates profiles.",
	}
	var quiet bool
	var since string
	var until string
	var all bool
	var waitBackoffs bool
	start := &cobra.Command{
		Use:   "start",
		Short: "Kick the worker in the background (or run a foreground backfill with --all).",
		Long:  "Kick the distillation worker in the background. Optional bounds select pending sessions by their latest raw timestamp; values accept RFC3339, YYYY-MM-DD (UTC), or an age such as 7d or 24h. If another worker already holds the lock, the new process exits and queued work remains durable on disk.\n\nWith --all, run the whole backlog in the FOREGROUND instead: this process drains every pending session (mining in parallel), loads the embedding model once, and blocks until done — the day-one \"distill my whole history\" path. --all cannot be combined with --since/--until.\n\nWith --all --wait-backoffs, also wait out any per-session mining backoffs (a timed-out or rate-limited session sleeps 5m, 10m, ... before retry) and re-drain, so \"all\" self-heals transient failures instead of returning \"backfill incomplete\" the moment the queue is momentarily empty of ready work. It gives up (and reports incomplete) once even the soonest retry is further out than a bounded wait — a session still failing by then is deterministic; re-run later to resume.",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			if all {
				return cmdDistillBackfill(quiet, since, until, waitBackoffs)
			}
			if waitBackoffs {
				return fmt.Errorf("--wait-backoffs applies only to the foreground backfill; use it with --all")
			}
			return cmdDistillStart(quiet, since, until)
		},
	}
	start.Flags().BoolVar(&quiet, "quiet", false, "suppress human-readable status output")
	start.Flags().StringVar(&since, "since", "", "latest session update at or after this time (for example 7d or 2026-07-01)")
	start.Flags().StringVar(&until, "until", "", "latest session update at or before this time")
	start.Flags().BoolVar(&all, "all", false, "drain the entire backlog in the foreground (blocks until done); the day-one backfill path")
	start.Flags().BoolVar(&waitBackoffs, "wait-backoffs", false, "with --all, wait out mining backoffs and retry timed-out/rate-limited sessions until the backlog drains or a retry is too far out")
	distillCmd.AddCommand(start)
	var statusJSON bool
	statusCmd := &cobra.Command{
		Use:   "status",
		Short: "Show worker and queue status.",
		Long:  "Show worker state, current session, archive statistics, pending/backoff counts, and raw/distilled freshness timestamps.",
		Args:  cobra.NoArgs,
		RunE:  func(_ *cobra.Command, _ []string) error { return cmdDistillStatus(statusJSON) },
	}
	statusCmd.Flags().BoolVarP(&statusJSON, "json", "j", false, "output as JSON")
	distillCmd.AddCommand(statusCmd)
	var stopAutoOnly bool
	stopCmd := &cobra.Command{
		Use:   "stop",
		Short: "Request the running worker to stop.",
		Long:  "Set the worker stop flag and send SIGTERM to the running worker process when it is still alive.",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			return cmdDistillStop(stopAutoOnly)
		},
	}
	stopCmd.Flags().BoolVar(&stopAutoOnly, "auto-only", false, "stop only an automatically-started worker")
	_ = stopCmd.Flags().MarkHidden("auto-only")
	distillCmd.AddCommand(stopCmd)
	return distillCmd
}

type sessionTimeRange struct {
	since time.Time
	until time.Time
}

func (r sessionTimeRange) empty() bool {
	return r.since.IsZero() && r.until.IsZero()
}

func cmdDistillStart(quiet bool, sinceValue, untilValue string) error {
	r, err := parseSessionTimeRange(sinceValue, untilValue, time.Now())
	if err != nil {
		return err
	}
	args := []string{"worker"}
	if !r.since.IsZero() {
		args = append(args, "--since", r.since.UTC().Format(time.RFC3339Nano))
	}
	if !r.until.IsZero() {
		args = append(args, "--until", r.until.UTC().Format(time.RFC3339Nano))
	}
	spawnDetached(args...)
	if !quiet {
		fmt.Println("distill worker kicked in the background; run `witness distill status` to watch progress")
	}
	return nil
}

// cmdDistillBackfill runs the whole pending backlog in the FOREGROUND (blocking),
// as opposed to cmdDistillStart's detached spawn. This is the day-one "distill my
// whole history" path: the embedder loads once for this process, the drain mines
// in parallel and re-checks until empty, and the exit code reflects success. It is
// still single-flight — if a background worker already holds the lock this no-ops
// with a message rather than running a second concurrent drain.
//
// --all is deliberately incompatible with --since/--until: a bounded backfill is
// just `distill start --since ...` (which the background path already supports);
// "all" means all.
//
// Contract (issue #22 review #2): a foreground backfill MUST NOT report success
// when the backlog was not actually drained. runWorkerInRange swallows an
// embedder-load failure and per-session mine/commit failures (they log or back off
// rather than propagate), so a nil error alone does NOT mean "done". We therefore
// inspect the END STATE after the drain and fail (nonzero exit) if any pending or
// backed-off work remains — so automation can detect an incomplete migration
// regardless of which internal layer failed.
//
// waitBackoffs (issue #56 B4) opts into self-healing transient mining failures: a
// timed-out/rate-limited session backs off 5m, 10m, ... and drops out of the READY
// queue, so a plain `--all` returns "backfill incomplete" the instant the queue is
// momentarily empty even though the session would succeed on retry. With the flag we
// sleep to the soonest NextBackoffAttempt and re-drain, looping until the backlog is
// clean OR the soonest retry is further out than backfillMaxWait (a session still
// failing by then is deterministic — e.g. a too-large session that times out every
// pass, whose real fix is chunking, #56 B1 blocked on #57 — so we stop and report
// incomplete rather than block the foreground indefinitely). The end-state check
// below stays the authority on success either way.
func cmdDistillBackfill(quiet bool, sinceValue, untilValue string, waitBackoffs bool) error {
	if strings.TrimSpace(sinceValue) != "" || strings.TrimSpace(untilValue) != "" {
		return fmt.Errorf("--all drains the entire backlog and cannot be combined with --since/--until")
	}
	if !quiet {
		fmt.Println("distilling the full backlog in the foreground; this may take a while — run `witness distill status` in another shell to watch")
	}
	// Snapshot the monotonic drift counter before the drain so we can report how many
	// prose_drift events THIS backfill produced (#57) — surfaced at the moment the user
	// runs it, not only on a later `witness doctor`.
	driftBefore := 0
	if st0, err := store.Open(); err == nil {
		driftBefore = st0.DriftTotal()
		st0.Close()
	}

	// A drain pass = one full foreground worker run (mine + review + re-check to empty).
	// It reports ran=false only when another worker already holds the lock.
	ran, err := runWorker(false)
	if err != nil {
		return err
	}
	if !ran {
		fmt.Println("another distillation worker is already running; it is draining the backlog — nothing to do")
		return nil
	}

	if waitBackoffs {
		// Ctrl-C during a between-passes sleep aborts the wait and falls through to the
		// end-state check, which reports the honest (still-incomplete) state.
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		if err := backfillDrainWithRetry(ctx, backfillRetryDeps{
			rerun:            func() error { _, e := runWorker(false); return e },
			waitForNextRetry: waitForNextBackfillRetry,
			sleep:            interruptibleSleep,
			maxWait:          backfillMaxWait,
			logWaiting:       func(d time.Duration) { backfillLogWaiting(quiet, d) },
		}); err != nil {
			return err
		}
	}

	// Verify the end state: nothing should be pending or backed off after a full
	// foreground drain. If work remains, mining did not complete (missing model,
	// provider outage, or commit error) — surface it as a failure, not "complete".
	st, err := store.Open()
	if err != nil {
		return err
	}
	defer st.Close()
	stats := st.Stats(activeLensNames(st))
	if remaining := stats.Pending + stats.BackedOff; remaining > 0 {
		hint := "a missing embedding model or provider failure is the usual cause"
		if stats.BackedOff > 0 && !waitBackoffs {
			hint = "re-run with `--all --wait-backoffs` to wait out transient mining backoffs"
		}
		return fmt.Errorf("backfill incomplete: %d session(s) still pending, %d backed off — mining did not finish (check `witness doctor` / witness.log; %s)", stats.Pending, stats.BackedOff, hint)
	}
	if !quiet {
		msg := "backfill complete"
		// A drifted lens still advances its watermark (so it's not "pending"), but it
		// distilled to zero observations — report it here so a below-floor triage model
		// is visible now, not silently a thin archive.
		if drifted := st.DriftTotal() - driftBefore; drifted > 0 {
			msg += fmt.Sprintf(" (%d session-lens drifted: model returned no observations — raise triage_model, then re-mine; see `witness doctor`)", drifted)
		}
		fmt.Println(msg)
	}
	return nil
}

// backfillMaxWait bounds how far out a mining backoff the foreground `--all
// --wait-backoffs` loop will wait between re-drains. Mining backoff doubles (5m, 10m,
// 20m, ... — backoffDelay in internal/distill), so 15m waits out the first two retries
// (attempt 1 → 5m, attempt 2 → 10m: transient outages) but stops once the soonest
// retry is 20m+ out, treating a session that has failed 3+ times as deterministically
// broken (its real fix isn't waiting longer). Because each failed session's backoff
// strictly grows and only clears on SUCCESS, the soonest retry time strictly advances
// every pass, so the loop provably terminates — either the queue drains or the min
// retry crosses this bound.
const backfillMaxWait = 15 * time.Minute

// backfillRetryDeps are the seams backfillDrainWithRetry needs, injected so the retry
// policy + termination are unit-testable without a real store, embedder, or clock
// (the same seam pattern as distillLoopDeps). waitForNextRetry returns the DURATION
// until the soonest outstanding backoff — a duration, not an absolute time, so the
// loop carries NO hidden dependency on the real clock (a test drives it with plain
// numbers).
type backfillRetryDeps struct {
	rerun            func() error                               // run one more full foreground drain pass
	waitForNextRetry func() (time.Duration, bool)               // time until the soonest backed-off retry; false = none outstanding
	sleep            func(ctx context.Context, d time.Duration) // wait (interruptibly) between passes
	maxWait          time.Duration                              // give up once the soonest retry is further out than this
	logWaiting       func(d time.Duration)                      // narrate a wait to the user
}

// backfillDrainWithRetry is the pure --wait-backoffs loop: while some session is still
// sleeping out a mining backoff AND its retry is within maxWait, sleep until it's due
// and drain again. Terminates when no backoff is outstanding (queue clean or only
// non-backoff work remains, which the end-state check judges) or the soonest retry is
// beyond maxWait (deterministic failure — stop and let the caller report incomplete).
// Returns only a rerun error; "still incomplete" is not an error here (the end-state
// check owns that verdict).
func backfillDrainWithRetry(ctx context.Context, d backfillRetryDeps) error {
	for {
		if ctx.Err() != nil {
			return nil // interrupted; the end-state check reports the honest state
		}
		wait, ok := d.waitForNextRetry()
		if !ok {
			return nil // nothing backed off → nothing to wait for
		}
		if wait > d.maxWait {
			return nil // soonest retry too far out → treat as deterministic; stop waiting
		}
		if wait > 0 {
			d.logWaiting(wait)
			d.sleep(ctx, wait)
			if ctx.Err() != nil {
				return nil
			}
		}
		if err := d.rerun(); err != nil {
			return err
		}
	}
}

// waitForNextBackfillRetry reports the DURATION until the soonest outstanding
// mining-backoff retry across the active lenses (the same active-set contract as the
// end-state Stats check), or ok=false if none is outstanding. A retry already due
// (next_attempt in the past) yields a non-positive duration, which the loop treats as
// "rerun now".
func waitForNextBackfillRetry() (time.Duration, bool) {
	st, err := store.Open()
	if err != nil {
		return 0, false
	}
	defer st.Close()
	next, ok := st.NextBackoffAttempt(activeLensNames(st), time.Now())
	if !ok {
		return 0, false
	}
	return time.Until(next), true
}

// interruptibleSleep waits d, returning early if ctx is cancelled (Ctrl-C).
func interruptibleSleep(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
	case <-ctx.Done():
	}
}

func backfillLogWaiting(quiet bool, d time.Duration) {
	if quiet {
		return
	}
	// Round to the second so the message is readable ("waiting 4m59s" not nanoseconds).
	fmt.Printf("backlog has backed-off sessions; waiting %s for the next retry (Ctrl-C to stop and report progress)\n", d.Round(time.Second))
}

func parseSessionTimeRange(sinceValue, untilValue string, now time.Time) (sessionTimeRange, error) {
	since, err := parseSessionTimeBound(sinceValue, now, false)
	if err != nil {
		return sessionTimeRange{}, fmt.Errorf("invalid --since: %w", err)
	}
	until, err := parseSessionTimeBound(untilValue, now, true)
	if err != nil {
		return sessionTimeRange{}, fmt.Errorf("invalid --until: %w", err)
	}
	if !since.IsZero() && !until.IsZero() && since.After(until) {
		return sessionTimeRange{}, fmt.Errorf("--since must not be later than --until")
	}
	return sessionTimeRange{since: since, until: until}, nil
}

func parseSessionTimeBound(value string, now time.Time, endOfDay bool) (time.Time, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return time.Time{}, nil
	}
	if t, err := time.Parse(time.RFC3339Nano, value); err == nil {
		return t, nil
	}
	if t, err := time.ParseInLocation("2006-01-02", value, time.UTC); err == nil {
		if endOfDay {
			return t.AddDate(0, 0, 1).Add(-time.Nanosecond), nil
		}
		return t, nil
	}
	age, err := parseSessionAge(value)
	if err != nil {
		return time.Time{}, fmt.Errorf("expected RFC3339, YYYY-MM-DD, or an age such as 7d or 24h")
	}
	return now.Add(-age), nil
}

func parseSessionAge(value string) (time.Duration, error) {
	unit := time.Duration(0)
	suffix := ""
	if strings.HasSuffix(value, "d") {
		unit, suffix = 24*time.Hour, "d"
	} else if strings.HasSuffix(value, "w") {
		unit, suffix = 7*24*time.Hour, "w"
	}
	if unit != 0 {
		n, err := strconv.ParseFloat(strings.TrimSuffix(value, suffix), 64)
		if err != nil || n <= 0 {
			return 0, fmt.Errorf("invalid age")
		}
		return time.Duration(n * float64(unit)), nil
	}
	d, err := time.ParseDuration(value)
	if err != nil || d <= 0 {
		return 0, fmt.Errorf("invalid age")
	}
	return d, nil
}

func cmdDistillStatus(asJSON bool) error {
	st, err := store.Open()
	if err != nil {
		return err
	}
	defer st.Close()
	stat := st.Stats(activeLensNames(st))
	// The flock is the SOLE liveness authority (issue #75): the OS drops it when the
	// holder dies — normal exit, SIGKILL, OOM, or power loss alike — so a crashed
	// worker reads as idle here with no `ps` probe and no stale-state self-heal. The
	// running/stopping sub-state is DERIVED, not stored: a live worker with a pending
	// worker_stop_requested is "stopping", otherwise "running". The remaining worker_*
	// meta is diagnostic only (pid/mode/heartbeat/current), trusted ONLY while a worker
	// is genuinely live; when idle the values may be stale from a past crash — the next
	// worker start overwrites them — so we surface nothing but "idle".
	active := st.WorkerActive()
	status := "idle"
	var pid, mode, heartbeat, current string
	if active {
		status = "running"
		if st.MetaString("worker_stop_requested") == "1" {
			status = "stopping"
		}
		pid = st.MetaString("worker_pid")
		mode = st.MetaString("worker_mode")
		heartbeat = st.MetaString("worker_heartbeat")
		current = st.MetaString("worker_current")
	}
	lastRaw := st.LastRawTS()
	lastDistilled := st.LastDistilledRawTS()
	if asJSON {
		out := distillStatusJSON{
			Worker: distillWorkerJSON{
				Status:    status,
				PID:       pid,
				Mode:      mode,
				Heartbeat: heartbeat,
				Current:   current,
			},
			Archive: distillArchiveJSON{
				Sessions:     stat.Sessions,
				RawRecords:   stat.RawRecords,
				Observations: stat.Observations,
				Facets:       stat.Facets,
			},
			Queue: distillQueueJSON{
				Pending:   stat.Pending,
				BackedOff: stat.BackedOff,
			},
			RawThrough:       valueOrNever(lastRaw),
			DistilledThrough: valueOrNever(lastDistilled),
		}
		return emitJSON(out)
	}
	// Decorative rendering (same fields as --json above). Glyph reflects worker
	// liveness: running/stopping = active, idle = neutral.
	workerGlyph := dim("○")
	statusText := status
	switch status {
	case "running":
		workerGlyph = green("●")
		statusText = green(status)
	case "stopping":
		workerGlyph = yellow("●")
		statusText = yellow(status)
	}
	fmt.Printf("%s %s %s", workerGlyph, bold("worker:"), statusText)
	if pid != "" {
		fmt.Printf("  %s", dim("pid="+pid))
	}
	if mode != "" {
		fmt.Printf("  %s", dim("mode="+mode))
	}
	if heartbeat != "" {
		fmt.Printf("  %s", dim("♥ "+heartbeat))
	}
	fmt.Println()
	if current != "" {
		fmt.Printf("  %s %s\n", label("current"), current)
	}
	fmt.Printf("  %s %d sessions · %d messages\n", label("raw"), stat.Sessions, stat.RawRecords)
	fmt.Printf("  %s %d observations · %d facets\n", label("distilled"), stat.Observations, stat.Facets)
	queueLine := fmt.Sprintf("%d pending · %d backing off", stat.Pending, stat.BackedOff)
	if stat.BackedOff > 0 {
		fmt.Printf("  %s %s\n", label("queue"), yellow(queueLine))
	} else {
		fmt.Printf("  %s %s\n", label("queue"), queueLine)
	}
	fmt.Printf("  %s raw %s  ·  distilled %s\n", label("through"),
		valueOrNever(lastRaw), valueOrNever(lastDistilled))
	return nil
}

type distillStatusJSON struct {
	Worker           distillWorkerJSON  `json:"worker"`
	Archive          distillArchiveJSON `json:"archive"`
	Queue            distillQueueJSON   `json:"queue"`
	RawThrough       string             `json:"raw_through"`
	DistilledThrough string             `json:"distilled_through"`
}

type distillWorkerJSON struct {
	Status    string `json:"status"`
	PID       string `json:"pid,omitempty"`
	Mode      string `json:"mode,omitempty"`
	Heartbeat string `json:"heartbeat,omitempty"`
	Current   string `json:"current,omitempty"`
}

type distillArchiveJSON struct {
	Sessions     int `json:"sessions"`
	RawRecords   int `json:"raw_records"`
	Observations int `json:"observations"`
	Facets       int `json:"facets"`
}

type distillQueueJSON struct {
	Pending   int `json:"pending"`
	BackedOff int `json:"backed_off"`
}

func cmdDistillStop(autoOnly bool) error {
	st, err := store.Open()
	if err != nil {
		return err
	}
	defer st.Close()
	if autoOnly {
		_ = clearScheduledWakeup(st, "auto")
	}
	if !autoOnly {
		_ = clearScheduledWakeup(st, "")
	}
	mode := st.MetaString("worker_mode")
	if autoOnly && mode == "manual" {
		return nil
	}
	if err := st.SetMetaString("worker_stop_requested", "1"); err != nil {
		return err
	}
	if autoOnly && mode != "auto" {
		return nil // cancels an auto worker that has been spawned but has not claimed the lock yet
	}
	// The flock is the liveness authority (issue #75): if no worker holds it, none is
	// running — no `ps` probe, no stale-meta self-heal (a crashed worker's flock was
	// already released by the OS).
	if !st.WorkerActive() {
		fmt.Println("distill worker is not running")
		return nil
	}
	// A worker is live. worker_pid is the kill TARGET, not the liveness authority:
	// signal it to tear down in-flight `claude -p`/`opencode` children now. The
	// worker_stop_requested flag set above is the durable backstop — so if pid is
	// missing or stale (a worker that just claimed the lock but hasn't stamped its pid
	// yet), the worker still honors the flag at its next checkpoint. We therefore
	// report the stop request rather than error when the signal can't be delivered.
	// worker_stop_requested=1 (set above) IS the "stopping" state now — cmdDistillStatus
	// derives it from that flag while the worker is live, so there's no separate status
	// key to update here.
	pid := st.MetaString("worker_pid")
	if err := terminateWorker(pid); err != nil {
		fmt.Println("distill stop requested; the running worker will exit at its next checkpoint")
		return nil
	}
	fmt.Printf("distill stop requested; sent TERM to worker pid=%s\n", pid)
	return nil
}

func terminateWorker(pid string) error {
	n, err := strconv.Atoi(strings.TrimSpace(pid))
	if err != nil || n <= 0 {
		return fmt.Errorf("invalid worker pid %q", pid)
	}
	// terminateWorkerPID is GOOS-split: on Unix it signals the worker's process
	// GROUP first (SIGTERM to -n, matching the setsid detach) then the pid; on
	// Windows there are no process-group signals, so it opens the process and
	// terminates it. See procsignal_unix.go / procsignal_windows.go.
	return terminateWorkerPID(n)
}
