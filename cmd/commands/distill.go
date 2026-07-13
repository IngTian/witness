package commands

import (
	"fmt"
	"os/exec"
	"strconv"
	"strings"
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
	start := &cobra.Command{
		Use:   "start",
		Short: "Kick the worker in the background (or run a foreground backfill with --all).",
		Long:  "Kick the distillation worker in the background. Optional bounds select pending sessions by their latest raw timestamp; values accept RFC3339, YYYY-MM-DD (UTC), or an age such as 7d or 24h. If another worker already holds the lock, the new process exits and queued work remains durable on disk.\n\nWith --all, run the whole backlog in the FOREGROUND instead: this process drains every pending session (mining in parallel), loads the embedding model once, and blocks until done — the day-one \"distill my whole history\" path. --all cannot be combined with --since/--until.",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			if all {
				return cmdDistillBackfill(quiet, since, until)
			}
			return cmdDistillStart(quiet, since, until)
		},
	}
	start.Flags().BoolVar(&quiet, "quiet", false, "suppress human-readable status output")
	start.Flags().StringVar(&since, "since", "", "latest session update at or after this time (for example 7d or 2026-07-01)")
	start.Flags().StringVar(&until, "until", "", "latest session update at or before this time")
	start.Flags().BoolVar(&all, "all", false, "drain the entire backlog in the foreground (blocks until done); the day-one backfill path")
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
func cmdDistillBackfill(quiet bool, sinceValue, untilValue string) error {
	if strings.TrimSpace(sinceValue) != "" || strings.TrimSpace(untilValue) != "" {
		return fmt.Errorf("--all drains the entire backlog and cannot be combined with --since/--until")
	}
	if !quiet {
		fmt.Println("distilling the full backlog in the foreground; this may take a while — run `witness distill status` in another shell to watch")
	}
	ran, err := runWorker(false)
	if err != nil {
		return err
	}
	if !ran {
		fmt.Println("another distillation worker is already running; it is draining the backlog — nothing to do")
		return nil
	}
	if !quiet {
		fmt.Println("backfill complete")
	}
	return nil
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
	stat := st.Stats()
	status := st.MetaString("worker_status")
	if status == "" {
		status = "idle"
	}
	pid := st.MetaString("worker_pid")
	mode := st.MetaString("worker_mode")
	heartbeat := st.MetaString("worker_heartbeat")
	current := st.MetaString("worker_current")
	if (status == "running" || status == "stopping") && !workerPIDAlive(pid) {
		status = "idle"
		pid = ""
		current = ""
		_ = st.SetMetaString("worker_status", "idle")
		_ = st.SetMetaString("worker_pid", "")
		_ = st.SetMetaString("worker_mode", "")
		_ = st.SetMetaString("worker_current", "")
	}
	if status == "idle" {
		pid = ""
		mode = ""
		current = ""
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
	pid := st.MetaString("worker_pid")
	if !workerPIDAlive(pid) {
		_ = st.SetMetaString("worker_status", "idle")
		_ = st.SetMetaString("worker_pid", "")
		_ = st.SetMetaString("worker_current", "")
		fmt.Println("distill worker is not running")
		return nil
	}
	if err := terminateWorker(pid); err != nil {
		return err
	}
	_ = st.SetMetaString("worker_status", "stopping")
	fmt.Printf("distill stop requested; sent TERM to worker pid=%s\n", pid)
	return nil
}

func workerPIDAlive(pid string) bool {
	n, err := strconv.Atoi(strings.TrimSpace(pid))
	if err != nil || n <= 0 {
		return false
	}
	return isWitnessWorkerProcess(n)
}

func isWitnessWorkerProcess(pid int) bool {
	out, err := exec.Command("ps", "-p", strconv.Itoa(pid), "-o", "command=").Output()
	if err != nil {
		return false
	}
	cmd := strings.TrimSpace(string(out))
	if !strings.Contains(cmd, "witness") {
		return false
	}
	// Match the `worker` subcommand but NOT its `worker-wakeup` sibling — a bare
	// `strings.Contains(cmd, " worker")` also matches "worker-wakeup", which would
	// make a liveness check treat a transient wakeup process as the worker (issue
	// #24). Accept `worker` only as a whole token: end of string, or followed by a
	// space (a flag/arg), never `worker-…`.
	for _, field := range strings.Fields(cmd) {
		if field == "worker" {
			return true
		}
	}
	return false
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
