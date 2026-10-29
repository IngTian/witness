package commands

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"

	"github.com/IngTian/witness/internal/platform"
	"github.com/IngTian/witness/internal/store"
	"github.com/spf13/cobra"
)

func newImportCmd() *cobra.Command {
	var agent string
	var sessions []string
	var quiet bool
	var auto bool
	var noKick bool
	var hook bool
	c := &cobra.Command{
		Use:    "import --agent <claude|opencode|codex>",
		Short:  "Import agent session data and kick background distillation.",
		Long:   "Import agent data into L0 raw records and kick the background distillation worker when work is pending. OpenCode imports its local SQLite database; Codex reads local JSONL rollouts or paginated history; Claude relies on already-captured hook data.",
		Args:   cobra.NoArgs,
		Hidden: true,
		RunE: func(_ *cobra.Command, _ []string) error {
			if hook {
				return cmdImportHook(agent, os.Stdin)
			}
			args := []string{"--agent", agent}
			for _, session := range sessions {
				args = append(args, "--session", session)
			}
			if quiet {
				args = append(args, "--quiet")
			}
			if auto {
				args = append(args, "--auto")
			}
			if noKick {
				args = append(args, "--no-kick")
			}
			return cmdImport(args)
		},
	}
	c.Flags().StringVar(&agent, "agent", "", "agent to import from: claude, opencode or codex")
	c.Flags().BoolVar(&hook, "hook", false, "best-effort import of session_id from a lifecycle hook on stdin")
	_ = c.Flags().MarkHidden("hook")
	c.Flags().StringArrayVar(&sessions, "session", nil, "native session id to import (repeatable)")
	c.Flags().BoolVar(&quiet, "quiet", false, "suppress human-readable status output")
	c.Flags().BoolVar(&auto, "auto", false, "use the automatic worker gate (only start if auto_distill is on and no worker is running)")
	_ = c.Flags().MarkHidden("auto")
	c.Flags().BoolVar(&noKick, "no-kick", false, "import L0 without starting a worker")
	_ = c.Flags().MarkHidden("no-kick")
	return c
}

func cmdImport(args []string) error {
	agent := ""
	var sessions []string
	quiet := false
	auto := false
	noKick := false
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--agent":
			if i+1 >= len(args) || strings.TrimSpace(args[i+1]) == "" {
				return fmt.Errorf("--agent requires a value")
			}
			agent = strings.ToLower(strings.TrimSpace(args[i+1]))
			i++
		case "--quiet":
			quiet = true
		case "--auto":
			auto = true
		case "--no-kick":
			noKick = true
		case "--session":
			if i+1 >= len(args) || strings.TrimSpace(args[i+1]) == "" {
				return fmt.Errorf("--session requires a value")
			}
			sessions = append(sessions, strings.TrimSpace(args[i+1]))
			i++
		default:
			return fmt.Errorf("usage: witness import --agent <claude|opencode|codex> [--session <id>]... [--quiet] [--auto] [--no-kick]")
		}
	}
	if agent == "" {
		return fmt.Errorf("usage: witness import --agent <claude|opencode|codex> [--session <id>]... [--quiet] [--auto] [--no-kick]")
	}
	stats, kicked, err := runImport(agent, sessions, !noKick, auto)
	if err != nil {
		return err
	}
	if !quiet {
		fmt.Printf("import %s: imported %d raw record(s) from %d session(s)\n", stats.Agent, stats.Records, stats.Sessions)
		if kicked {
			fmt.Println("distill worker kicked in the background; run `witness status` to watch progress")
		} else if noKick {
			fmt.Println("background distillation not requested (--no-kick)")
		} else {
			fmt.Println("no distill work pending")
		}
	}
	return nil
}

var importWorkerSpawner = func() { spawnDetached("worker-run") }

func runImport(agent string, sessionIDs []string, kickWorker, auto bool) (platform.ImportStats, bool, error) {
	st, err := store.Open()
	if err != nil {
		return platform.ImportStats{}, false, err
	}
	defer st.Close()
	defer setupLogging(st)()

	p, ok := platform.ByName(agent)
	if !ok {
		return platform.ImportStats{}, false, fmt.Errorf("unknown import agent %q (want claude, opencode or codex)", agent)
	}
	// The Importer owns its own reconcile mechanics (OpenCode takes the sync lock
	// and reads its SQLite store; Claude is a hook-fed no-op) — cmd no longer
	// branches on the runtime.
	stats, err := p.Import(context.Background(), st, sessionIDs)
	if err != nil {
		return stats, false, err
	}

	cfg := st.LoadConfig()
	pending, _ := st.PendingSessions(activeLensNames(st))
	shouldRunWorker := len(pending) > 0 || st.ReviewDue(cfg)
	if kickWorker && shouldRunWorker {
		if auto {
			return stats, maybeSpawnAutoWorker(st), nil
		}
		importWorkerSpawner()
		return stats, true, nil
	}
	return stats, false, nil
}

// cmdImportHook is a best-effort reconcile trigger, not a transcript reader.
// Stable session_id selects the source adapter; the worker's usual auto gate
// applies. Hook failures never block a session and Stop receives valid JSON.
func cmdImportHook(agent string, input io.Reader) error {
	defer fmt.Println("{}")
	st, err := store.Open()
	if err != nil {
		return nil
	}
	defer st.Close()
	defer setupLogging(st)()
	var event struct {
		SessionID string `json:"session_id"`
	}
	if err := json.NewDecoder(io.LimitReader(input, 4<<20)).Decode(&event); err != nil {
		slog.Warn("import hook: invalid payload", "err", err)
		return nil
	}
	if strings.TrimSpace(event.SessionID) == "" {
		return nil
	}
	p, ok := platform.ByName(agent)
	if !ok {
		slog.Warn("import hook: unknown agent", "agent", agent)
		return nil
	}
	if _, err := p.Import(context.Background(), st, []string{event.SessionID}); err != nil {
		slog.Warn("import hook failed", "agent", agent, "err", err)
		return nil
	}
	maybeSpawnAutoWorker(st)
	return nil
}
