package commands

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"strings"

	"github.com/IngTian/witness/internal/distill"
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
		Use:    "import --agent <" + importAgents("|") + ">",
		Short:  "Import agent session data and kick background distillation.",
		Long:   "Import agent data into L0 raw records and kick the background distillation worker when work is pending. OpenCode imports its local SQLite database; Codex reads local JSONL rollouts or paginated history; Claude relies on already-captured hook data.",
		Args:   cobra.NoArgs,
		Hidden: true,
		RunE: func(_ *cobra.Command, _ []string) error {
			if hook {
				return cmdImportHook(agent, os.Stdin)
			}
			var args []string
			if agent != "" { // a bare `witness import` asks "which agent?" instead of "--agent requires a value"
				args = append(args, "--agent", agent)
			}
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
	c.Flags().StringVar(&agent, "agent", "", "agent to import from: "+importAgents(", "))
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
			return fmt.Errorf("usage: witness import --agent <%s> [--session <id>]... [--quiet] [--auto] [--no-kick]", importAgents("|"))
		}
	}
	if agent == "" {
		return fmt.Errorf("which agent? witness import --agent <%s>", importAgents("|"))
	}
	ctx, finish := context.Background(), func() {}
	if !quiet { // --quiet is how the OpenCode plugin imports in the background: it stays silent
		ctx, finish = importProgress(ctx, agent)
	}
	stats, kicked, err := runImport(ctx, agent, sessions, !noKick, auto)
	finish()
	if err != nil {
		return err
	}
	if !quiet {
		printImportResult(stats, kicked, noKick)
	}
	return nil
}

// importAgents lists the agents import takes — the editors, in the one order wire uses
// (platform.InstallTargets). The "file" source is pushed in by `witness ingest`; its import was a
// silent no-op that reported "imported 0 raw record(s)" although the usage never offered it.
func importAgents(sep string) string { return strings.Join(platform.InstallTargets(), sep) }

func printImportResult(stats platform.ImportStats, kicked, noKick bool) {
	name := bold(stats.Agent)
	note := ""
	switch {
	case kicked:
		note = " " + dim("(distilling in the background)")
	case noKick:
		note = " " + dim("(not distilling: --no-kick)")
	}
	glyph := okGlyph()
	if stats.Failed > 0 {
		glyph = warnGlyph()
	}
	hookFed := false
	if p, ok := platform.ByName(stats.Agent); ok {
		_, hookFed = p.(platform.Capturer)
	}
	switch {
	case hookFed && stats.Records == 0 && stats.Failed == 0:
		// A hook-fed source has nothing to pull: `import --agent claude` reported "imported 0 raw
		// record(s)" as if it had looked and found nothing.
		fmt.Printf("%s %s: nothing to import — it is captured live by its hooks%s\n", skipGlyph(), name, note)
		fmt.Println(hint("wire it", "witness wire "+stats.Agent))
	case stats.Records == 0:
		fmt.Printf("%s %s: up to date — nothing new since the last import%s\n", glyph, name, note)
	default:
		fmt.Printf("%s %s: imported %s from %s%s\n", glyph, name, countOf(stats.Records, "record", "records"),
			countOf(stats.Sessions, "session", "sessions"), note)
	}
	if stats.Failed > 0 {
		fmt.Println(dim("  skipped " + pluralize(stats.Failed, "source input", "source inputs") + " with errors — the rest were kept; details in witness.log"))
	}
	if kicked {
		fmt.Println(hint("watch it", "witness status"))
	}
}

// importProgress puts a progress line (stderr) under ctx for the importer to drive, and returns the
// call that clears it — made before the summary prints, so the two never share a terminal row. An
// import with nothing to read (Claude and file import nothing; an empty Codex home) draws nothing.
func importProgress(ctx context.Context, agent string) (context.Context, func()) {
	pl := startImportProgress()
	return platform.WithImportProgress(ctx, func(done, total int) {
		if total > 0 {
			pl.update(distill.Progress{Where: agent, Stages: []distill.Stage{{Name: "importing", Done: done, Total: total}}})
		}
	}), pl.finish
}

// startImportProgress is swapped by a test for a terminal-mode line, which no test process has.
var startImportProgress = startProgress

var importWorkerSpawner = func() { spawnDetached("worker-run") }

func runImport(ctx context.Context, agent string, sessionIDs []string, kickWorker, auto bool) (platform.ImportStats, bool, error) {
	st, err := store.Open()
	if err != nil {
		return platform.ImportStats{}, false, err
	}
	defer st.Close()
	defer setupLogging(st)()

	p, ok := platform.ByName(agent)
	if _, editor := platform.InstallerFor(agent); !ok || !editor {
		return platform.ImportStats{}, false, fmt.Errorf("unknown agent %q — want one of: %s", agent, importAgents(", "))
	}
	// The Importer owns its own reconcile mechanics (OpenCode takes the sync lock
	// and reads its SQLite store; hook-fed Claude has no Importer and imports
	// nothing) — cmd no longer branches on the runtime.
	stats, err := platform.ImportFor(ctx, p, st, sessionIDs)
	if err != nil {
		return stats, false, missingSource(agent, err)
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

// missingSource names an importer's absent source plainly. The importers wrap the stat error, so
// it read "open opencode db /x/opencode.db: stat /x/opencode.db: no such file or directory" — the
// path twice, and no word on what to do. Any other error passes through unchanged. Worded from the
// agent's name and the error's own path, so no venue is special-cased here.
func missingSource(agent string, err error) error {
	var pe *fs.PathError
	if !errors.Is(err, fs.ErrNotExist) || !errors.As(err, &pe) {
		return err
	}
	return fmt.Errorf("nothing to import: %s does not exist — is %s installed here?", tildePath(pe.Path), agent)
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
	stats, err := platform.ImportFor(context.Background(), p, st, []string{event.SessionID})
	if err != nil {
		slog.Warn("import hook failed", "agent", agent, "err", err)
		return nil
	}
	if stats.Failed > 0 {
		slog.Warn("import hook: some source inputs failed; successful imports kept", "agent", agent, "failed", stats.Failed)
	}
	maybeSpawnAutoWorker(st)
	return nil
}
