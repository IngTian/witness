package commands

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"time"

	"github.com/IngTian/witness/internal/platform"
	"github.com/IngTian/witness/internal/store"
	"github.com/spf13/cobra"
)

func newInternalCaptureCmd() *cobra.Command {
	var agent string
	c := &cobra.Command{
		Use:    "capture",
		Short:  "Internal hook entry point for raw capture.",
		Hidden: true,
		RunE: func(_ *cobra.Command, _ []string) error {
			args := []string{}
			if agent != "" {
				args = append(args, "--agent", agent)
			}
			return cmdCapture(args)
		},
	}
	c.Flags().StringVar(&agent, "agent", string(platform.AgentClaude), "internal capture agent")
	return c
}

// cmdCapture writes one raw record from the hook event. Pure plumbing; no LLM.
// Best-effort: it logs failures (so they're diagnosable) but always returns nil
// so a capture problem never breaks the user's session.
func cmdCapture(args []string) error {
	agent, err := agentFlag(args, platform.AgentClaude)
	if err != nil {
		return err
	}
	st, err := store.Open()
	if err != nil {
		return nil
	}
	defer st.Close()
	defer setupLogging(st)()

	data, err := io.ReadAll(os.Stdin)
	if err != nil {
		slog.Warn("capture: unreadable hook event", "err", err)
		return nil
	}
	p, ok := platform.ByName(agent)
	if !ok {
		return fmt.Errorf("unknown capture agent %q", agent)
	}
	capturer, ok := p.(platform.Capturer)
	if !ok {
		slog.Warn("capture: agent does not support event capture", "agent", agent)
		return nil
	}
	if _, err := capturer.Capture(st, data, time.Now()); err != nil {
		// Best-effort: a malformed payload or write error is logged, never fatal —
		// capture must never break the user's session.
		slog.Error("capture: event failed", "agent", agent, "err", err)
	}
	return nil
}
