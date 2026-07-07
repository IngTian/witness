package commands

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"time"

	"github.com/IngTian/witness/internal/runtimes"
	runtimeclaude "github.com/IngTian/witness/internal/runtimes/claude"
	opencodeimport "github.com/IngTian/witness/internal/runtimes/opencode"
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
	c.Flags().StringVar(&agent, "agent", string(runtimes.AgentClaude), "internal capture agent")
	return c
}

// cmdCapture writes one raw record from the hook event. Pure plumbing; no LLM.
// Best-effort: it logs failures (so they're diagnosable) but always returns nil
// so a capture problem never breaks the user's session.
func cmdCapture(args []string) error {
	agent, err := agentFlag(args, runtimes.AgentClaude)
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
	switch agent {
	case runtimes.AgentClaude:
		var e runtimeclaude.HookEvent
		if err := json.Unmarshal(data, &e); err != nil {
			slog.Warn("capture: unreadable claude hook event", "err", err)
			return nil
		}
		if err := runtimeclaude.Capture(st, e, time.Now()); err != nil {
			slog.Error("capture: append raw failed", "agent", agent, "session", e.SessionID, "err", err)
		}
	case runtimes.AgentOpenCode:
		if _, err := opencodeimport.Capture(st, data, time.Now()); err != nil {
			slog.Error("capture: opencode event failed", "err", err)
		}
	default:
		return fmt.Errorf("unknown capture agent %q", agent)
	}
	return nil
}
