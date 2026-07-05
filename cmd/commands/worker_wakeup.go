package commands

import (
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"github.com/IngTian/claude-witness/internal/store"
	"github.com/spf13/cobra"
)

func newInternalWorkerWakeupCmd() *cobra.Command {
	return &cobra.Command{
		Use:    "worker-wakeup <seconds>",
		Hidden: true,
		Args:   cobra.ExactArgs(1),
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
	_, err = runWorker()
	return err
}

func scheduleRetryWakeup(st *store.Store) {
	next, ok := st.NextBackoffAttempt(time.Now())
	if !ok {
		return
	}
	stamp := next.UTC().Format(time.RFC3339)
	if st.MetaString("worker_wakeup_at") == stamp {
		return
	}
	_ = st.SetMetaString("worker_wakeup_at", stamp)
	delay := time.Until(next)
	if delay < 0 {
		delay = 0
	}
	seconds := int(delay/time.Second) + 1
	spawnDetached("worker-wakeup", strconv.Itoa(seconds))
	slog.Info("distill: scheduled retry wakeup", "at", stamp, "delay", delay.String())
}
