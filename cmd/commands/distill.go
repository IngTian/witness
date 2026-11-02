package commands

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/IngTian/witness/internal/store"
)

// distill.go holds the cmd* implementation functions for distillation worker control.
// The old top-level `distill` command was removed from the visible front door (#102);
// the functionality migrated to the hidden `worker` group and `status` command, but
// the implementation functions (cmdDistillStart/Backfill/Status/Stop) remain here,
// called by worker_group.go and status.go.

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
	args := []string{"worker-run"}
	if !r.since.IsZero() {
		args = append(args, "--since", r.since.UTC().Format(time.RFC3339Nano))
	}
	if !r.until.IsZero() {
		args = append(args, "--until", r.until.UTC().Format(time.RFC3339Nano))
	}
	spawnDetached(args...)
	if !quiet {
		fmt.Println(bgGlyph() + " worker started in the background")
		fmt.Println(hint("watch it", "witness status"))
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
		// Name the flag the USER typed and the thing that actually works. This used to say
		// "--all drains the entire backlog and cannot be combined with --since/--until", naming a
		// `--all` flag that does not exist on `witness worker run` — so a user who typed
		// `worker run --since 7d` (a flag this command registers and documents) got an error about
		// a flag they had never heard of, with no hint that adding --detach makes it work.
		return fmt.Errorf("the foreground drain covers the whole backlog and cannot be limited with " +
			"--since/--until; add --detach to drain a time range in the background")
	}
	// Snapshot the monotonic drift counter before the drain so we can report how many
	// prose_drift events THIS backfill produced (#57) — surfaced at the moment the user
	// runs it, not only on a later `witness doctor`. The same open checks the lock BEFORE any
	// banner: this used to announce "distilling the full backlog" and only then find that
	// another worker held it.
	driftBefore := 0
	if st0, err := store.Open(); err == nil {
		driftBefore = st0.DriftTotal()
		busy := st0.WorkerActive()
		if busy && !quiet {
			fmt.Print(workerBusy(st0, "the backlog is already draining"))
		}
		st0.Close()
		if busy {
			return nil
		}
	}
	if !quiet {
		fmt.Println(dim("distilling the backlog in the foreground — Ctrl-C stops it"))
	}

	// The whole backlog can be hundreds of sessions and hours of model calls, so the drain shows
	// sessions done/total on stderr — unless --quiet, which asked for no human-readable status.
	progressWhere := "backlog"
	if quiet {
		progressWhere = ""
	}
	// tap keeps the first runner/model error, so an unfinished drain can say why on the screen.
	tap := &causeTap{}
	// A drain pass = one full foreground worker run (mine + review + re-check to empty).
	// It reports ran=false only when another worker already holds the lock.
	ran, err := runWorkerTapped(false, sessionTimeRange{}, progressWhere, tap)
	if err != nil {
		return err
	}
	if !ran { // lost the race for the lock after the check above
		if st1, err := store.Open(); err == nil {
			if !quiet {
				fmt.Print(workerBusy(st1, "the backlog is already draining"))
			}
			st1.Close()
		}
		return nil
	}

	if waitBackoffs {
		// Ctrl-C during a between-passes sleep aborts the wait and falls through to the
		// end-state check, which reports the honest (still-incomplete) state.
		ctx, stop := procCtl.NotifyStop(context.Background())
		defer stop()
		if err := backfillDrainWithRetry(ctx, backfillRetryDeps{
			rerun:            func() error { _, e := runWorkerTapped(false, sessionTimeRange{}, progressWhere, tap); return e },
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
	if stats.Pending+stats.BackedOff > 0 {
		return backlogUnfinished(stats.Pending, stats.BackedOff, waitBackoffs, tap.cause())
	}
	if !quiet {
		fmt.Println(okGlyph() + " backlog distilled")
		// A drifted lens still advances its watermark (so it's not "pending"), but it
		// distilled to zero observations — report it here so a below-floor mine model
		// is visible now, not silently a thin archive.
		if drifted := st.DriftTotal() - driftBefore; drifted > 0 {
			fmt.Println(dim(fmt.Sprintf("  %s drifted: the model returned no observations — raise mine_model, then re-mine",
				pluralize(drifted, "session-lens", "session-lenses"))))
			fmt.Println(hint("see which", "witness doctor"))
		}
	}
	return nil
}

// backlogUnfinished is the failure a foreground drain reports when work remains: what is left, the
// first error the drain hit, and only next steps that exist. It used to suggest `--all
// --wait-backoffs`; `witness worker run` has no --all, so following the hint failed as well.
func backlogUnfinished(pending, backedOff int, waited bool, cause string) error {
	var parts []string
	if backedOff > 0 {
		parts = append(parts, pluralize(backedOff, "session", "sessions")+" backed off")
	}
	if pending > 0 {
		parts = append(parts, pluralize(pending, "session", "sessions")+" still pending")
	}
	if cause == "" {
		cause = "see witness.log — usually a missing embedding model or a provider outage"
	}
	retry := ""
	if backedOff > 0 && !waited {
		retry = errHint("retry them", "witness worker run --wait-backoffs")
	}
	return errLines("backlog not finished: "+strings.Join(parts, ", "),
		errRow("cause", cause), retry, errHint("diagnose", "witness doctor"))
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
	fmt.Println(dim(fmt.Sprintf("waiting %s to retry the backed-off sessions — Ctrl-C stops and reports", d.Round(time.Second))))
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
	// Decorative rendering (same fields as --json above): the house title, then kvRow rows like
	// every other read command. The worker glyph is enabledGlyph's (ASCII in a pipe); stopping is
	// the one state it has no mark for, so it is yellow on a terminal.
	fmt.Println(title("status", "", ""))
	fmt.Println()
	workerText := enabledGlyph(false) + " " + status
	switch status {
	case "running":
		workerText = enabledGlyph(true) + " " + green(status)
	case "stopping":
		workerText = "* " + status
		if useColor {
			workerText = yellow("● " + status)
		}
	}
	var about []string
	if pid != "" {
		about = append(about, "pid "+pid)
	}
	if mode != "" {
		about = append(about, mode)
	}
	if heartbeat != "" {
		about = append(about, "heartbeat "+shortTime(heartbeat))
	}
	fmt.Println("  " + kvRow("worker", workerText, strings.Join(about, " · ")))
	if current != "" {
		fmt.Println("  " + kvRow("current", current, ""))
	}
	fmt.Println("  " + kvRow("raw", humanCount(stat.Sessions)+" sessions · "+humanCount(stat.RawRecords)+" messages", ""))
	fmt.Println("  " + kvRow("distilled", humanCount(stat.Observations)+" observations · "+humanCount(stat.Facets)+" facets", ""))
	queueLine := humanCount(stat.Pending) + " pending · " + humanCount(stat.BackedOff) + " backing off"
	if stat.BackedOff > 0 {
		queueLine = yellow(queueLine)
	}
	fmt.Println("  " + kvRow("queue", queueLine, ""))
	fmt.Println("  " + kvRow("fresh", "raw "+timeOrNever(lastRaw)+" · distilled "+timeOrNever(lastDistilled), behind(lastRaw, lastDistilled)))
	fmt.Println()
	switch {
	case stat.Sessions == 0:
		fmt.Println(warnGlyph() + " nothing captured yet — connect an editor")
		fmt.Println(hint("connect", "witness wire claude"))
	case stat.BackedOff > 0:
		fmt.Println(warnGlyph() + " " + pluralize(stat.BackedOff, "session", "sessions") + " backing off — mining is failing")
		fmt.Println(hint("diagnose", "witness doctor"))
	default:
		fmt.Println(hint("deep check", "witness doctor"))
	}
	return nil
}

// timeOrNever is shortTime for a stamp that may be empty.
func timeOrNever(ts string) string {
	if strings.TrimSpace(ts) == "" {
		return "never"
	}
	return shortTime(ts)
}

// behind says how far distillation trails capture, from the two status stamps: "1h05m behind",
// "caught up", or "" when either is missing or unreadable.
func behind(raw, distilled string) string {
	r, err1 := time.Parse(time.RFC3339, raw)
	d, err2 := time.Parse(time.RFC3339, distilled)
	if err1 != nil || err2 != nil {
		return ""
	}
	lag := r.Sub(d)
	switch {
	case lag < time.Minute:
		return "caught up"
	case lag < time.Hour:
		return fmt.Sprintf("%dm behind", int(lag.Minutes()))
	case lag < 48*time.Hour:
		return fmt.Sprintf("%dh%02dm behind", int(lag.Hours()), int(lag.Minutes())%60)
	default:
		return fmt.Sprintf("%dd behind", int(lag.Hours()/24))
	}
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
		fmt.Println(skipGlyph() + " no worker is running — nothing to stop")
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
		fmt.Println(okGlyph() + " stop requested — the worker exits at its next checkpoint")
		return nil
	}
	fmt.Println(okGlyph() + " stop requested — sent TERM to worker pid " + pid)
	return nil
}

func terminateWorker(pid string) error {
	n, err := strconv.Atoi(strings.TrimSpace(pid))
	if err != nil || n <= 0 {
		return fmt.Errorf("invalid worker pid %q", pid)
	}
	// proc.TerminateGroup is GOOS-split behind the port: on Unix it signals the
	// worker's process GROUP first (SIGTERM to -n, matching the setsid detach) then
	// the pid; on Windows there are no process-group signals, so it opens the process
	// and terminates it.
	return procCtl.TerminateGroup(n)
}
