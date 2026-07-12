package commands

import (
	"context"
	"fmt"
	"strings"

	"github.com/IngTian/witness/internal/platform"
	"github.com/IngTian/witness/internal/store"
	"github.com/spf13/cobra"
)

func newImportCmd() *cobra.Command {
	var agent string
	var quiet bool
	var auto bool
	c := &cobra.Command{
		Use:   "import --agent <claude|opencode>",
		Short: "Import agent session data and kick background distillation.",
		Long:  "Import agent data into L0 raw records and kick the background distillation worker when work is pending. OpenCode imports from its local SQLite session database; Claude relies on already-captured hook data.",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			args := []string{"--agent", agent}
			if quiet {
				args = append(args, "--quiet")
			}
			if auto {
				args = append(args, "--auto")
			}
			return cmdImport(args)
		},
	}
	c.Flags().StringVar(&agent, "agent", "", "agent to import from: claude or opencode")
	c.Flags().BoolVar(&quiet, "quiet", false, "suppress human-readable status output")
	c.Flags().BoolVar(&auto, "auto", false, "apply automatic distillation cooldown and session budget")
	_ = c.Flags().MarkHidden("auto")
	return c
}

func cmdImport(args []string) error {
	agent := ""
	quiet := false
	auto := false
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
		default:
			return fmt.Errorf("usage: witness import --agent <claude|opencode> [--quiet] [--auto]")
		}
	}
	if agent == "" {
		return fmt.Errorf("usage: witness import --agent <claude|opencode> [--quiet] [--auto]")
	}
	stats, kicked, err := runImport(agent, true, auto)
	if err != nil {
		return err
	}
	if !quiet {
		fmt.Printf("import %s: imported %d raw record(s) from %d session(s)\n", stats.Agent, stats.Records, stats.Sessions)
		if kicked {
			fmt.Println("distill worker kicked in the background; run `witness distill status` to watch progress")
		} else {
			fmt.Println("no distill work pending")
		}
	}
	return nil
}

func runImport(agent string, kickWorker, auto bool) (platform.ImportStats, bool, error) {
	st, err := store.Open()
	if err != nil {
		return platform.ImportStats{}, false, err
	}
	defer st.Close()
	defer setupLogging(st)()

	p, ok := platform.ByName(agent)
	if !ok {
		return platform.ImportStats{}, false, fmt.Errorf("unknown import agent %q (want claude or opencode)", agent)
	}
	// The Importer owns its own reconcile mechanics (OpenCode takes the sync lock
	// and reads its SQLite store; Claude is a hook-fed no-op) — cmd no longer
	// branches on the runtime.
	stats, err := p.Import(context.Background(), st)
	if err != nil {
		return stats, false, err
	}

	cfg := st.LoadConfig()
	pending, _ := st.PendingSessions()
	shouldRunWorker := len(pending) > 0 || st.ReviewDue(cfg)
	if kickWorker && shouldRunWorker {
		if auto {
			return stats, maybeSpawnAutoWorker(st), nil
		}
		spawnDetached("worker")
		return stats, true, nil
	}
	return stats, false, nil
}
