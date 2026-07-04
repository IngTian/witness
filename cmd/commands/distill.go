package commands

import (
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"syscall"

	"github.com/IngTian/claude-witness/internal/store"
	"github.com/spf13/cobra"
)

func newDistillCmd() *cobra.Command {
	distillCmd := &cobra.Command{
		Use:   "distill",
		Short: "Manage the background distillation worker.",
		Long:  "Start, inspect, or stop the single-flight worker that turns raw turns into observations, reviews facets when due, and regenerates profiles.",
	}
	var quiet bool
	start := &cobra.Command{
		Use:   "start",
		Short: "Kick the worker in the background.",
		Long:  "Kick the distillation worker in the background. If another worker already holds the lock, the new process exits and queued work remains durable on disk.",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			args := []string{"start"}
			if quiet {
				args = append(args, "--quiet")
			}
			return cmdDistill(args)
		},
	}
	start.Flags().BoolVar(&quiet, "quiet", false, "suppress human-readable status output")
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
	distillCmd.AddCommand(&cobra.Command{
		Use:   "stop",
		Short: "Request the running worker to stop.",
		Long:  "Set the worker stop flag and send SIGTERM to the running worker process when it is still alive.",
		Args:  cobra.NoArgs,
		RunE:  func(_ *cobra.Command, _ []string) error { return cmdDistill([]string{"stop"}) },
	})
	return distillCmd
}

func cmdDistill(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: witness distill <start|status|stop> [--quiet]")
	}
	switch args[0] {
	case "start":
		quiet := len(args) > 1 && args[1] == "--quiet"
		if len(args) > 2 || (len(args) == 2 && !quiet) {
			return fmt.Errorf("usage: witness distill start [--quiet]")
		}
		spawnDetached("worker")
		if !quiet {
			fmt.Println("distill worker kicked in the background; run `witness distill status` to watch progress")
		}
		return nil
	case "status":
		return cmdDistillStatus(false)
	case "stop":
		return cmdDistillStop()
	default:
		return fmt.Errorf("unknown distill subcommand %q (want start|status|stop)", args[0])
	}
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
	heartbeat := st.MetaString("worker_heartbeat")
	current := st.MetaString("worker_current")
	if (status == "running" || status == "stopping") && !workerPIDAlive(pid) {
		status = "idle"
		pid = ""
		current = ""
		_ = st.SetMetaString("worker_status", "idle")
		_ = st.SetMetaString("worker_pid", "")
		_ = st.SetMetaString("worker_current", "")
	}
	if status == "idle" {
		pid = ""
		current = ""
	}
	lastRaw := st.LastRawTS()
	lastDistilled := st.LastDistilledRawTS()
	if asJSON {
		out := distillStatusJSON{
			Worker: distillWorkerJSON{
				Status:    status,
				PID:       pid,
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

func cmdDistillStop() error {
	st, err := store.Open()
	if err != nil {
		return err
	}
	defer st.Close()
	if err := st.SetMetaString("worker_stop_requested", "1"); err != nil {
		return err
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
	return strings.Contains(cmd, "witness") && strings.Contains(cmd, " worker")
}

func terminateWorker(pid string) error {
	n, err := strconv.Atoi(strings.TrimSpace(pid))
	if err != nil || n <= 0 {
		return fmt.Errorf("invalid worker pid %q", pid)
	}
	if err := syscall.Kill(-n, syscall.SIGTERM); err == nil {
		return nil
	}
	if err := syscall.Kill(n, syscall.SIGTERM); err != nil {
		return fmt.Errorf("terminate worker pid=%d: %w", n, err)
	}
	return nil
}
