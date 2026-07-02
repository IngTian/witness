// Command witness is the single self-contained binary behind the claude-witness
// plugin: capture, the detached worker, the periodic reviewer, and the MCP server,
// dispatched by subcommand. Built pure-Go (CGO_ENABLED=0) — see internal/embed.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/IngTian/claude-witness/internal/distill"
	"github.com/IngTian/claude-witness/internal/embed"
	"github.com/IngTian/claude-witness/internal/lens"
	"github.com/IngTian/claude-witness/internal/mcp"
	opencodeimport "github.com/IngTian/claude-witness/internal/opencode"
	"github.com/IngTian/claude-witness/internal/store"
)

func main() {
	if len(os.Args) < 2 {
		// Only the human-facing commands are advertised. The rest are internal entry
		// points invoked by Claude Code, never typed: capture/session-start/session-end
		// (hooks), worker (self-spawned via spawnDetached), and mcp (the server process
		// Claude Code launches from the registered shim command).
		fmt.Fprintln(os.Stderr, "usage: witness <doctor|profile|review|lens|opencode|cleanup|install|uninstall> [args]")
		os.Exit(2)
	}
	// Belt-and-suspenders recursion guard (the shim also checks): never act when
	// running inside a witness-driven `claude -p`.
	if os.Getenv("WITNESS_WORKER") == "1" && os.Args[1] != "doctor" {
		_, _ = io.Copy(io.Discard, os.Stdin)
		return
	}

	var err error
	switch os.Args[1] {
	// Internal entry points (Claude Code / hooks / self-spawn) — not in usage.
	case "capture":
		err = cmdCapture()
	case "session-start":
		err = cmdSessionStart()
	case "session-end":
		err = cmdSessionEnd()
	case "worker":
		err = cmdWorker(os.Args[2:])
	case "mcp":
		err = cmdMCP()
	case "opencode-sync":
		err = cmdOpenCodeSync(os.Args[2:], true)
	// Human commands.
	case "profile":
		err = cmdProfile(os.Args[2:])
	case "review":
		err = cmdReview()
	case "lens":
		err = cmdLens(os.Args[2:])
	case "opencode":
		err = cmdOpenCode(os.Args[2:])
	case "install":
		err = cmdInstall(os.Args[2:])
	case "uninstall":
		err = cmdUninstall(os.Args[2:])
	case "cleanup":
		err = cmdCleanup()
	case "doctor":
		err = cmdDoctor()
	default:
		fmt.Fprintf(os.Stderr, "unknown subcommand %q\n", os.Args[1])
		os.Exit(2)
	}
	if err != nil {
		// Hooks must never break the session: log to stderr, exit 0 for capture-ish
		// paths. Only doctor/mcp surface failures.
		fmt.Fprintln(os.Stderr, "witness:", err)
		switch os.Args[1] {
		case "doctor", "mcp", "worker", "lens", "profile", "review", "opencode", "install", "uninstall", "cleanup":
			os.Exit(1)
		}
	}
}

// hookEvent is the subset of the hook JSON payload we read from stdin.
type hookEvent struct {
	HookEventName        string         `json:"hook_event_name"`
	SessionID            string         `json:"session_id"`
	Prompt               string         `json:"prompt"`                 // UserPromptSubmit
	LastAssistantMessage string         `json:"last_assistant_message"` // Stop
	Effort               map[string]any `json:"effort"`                 // Stop
	Cwd                  string         `json:"cwd"`
}

func readEvent() (hookEvent, error) {
	var e hookEvent
	data, err := io.ReadAll(os.Stdin)
	if err != nil {
		return e, err
	}
	if err := json.Unmarshal(data, &e); err != nil {
		return e, err // surfaced to the caller so it can be logged, not swallowed
	}
	return e, nil
}

// cmdCapture writes one raw record from the hook event. Pure plumbing; no LLM.
// Best-effort: it logs failures (so they're diagnosable) but always returns nil
// so a capture problem never breaks the user's session.
func cmdCapture() error {
	st, err := store.Open()
	if err != nil {
		return nil
	}
	defer st.Close()
	defer setupLogging(st)()

	e, err := readEvent()
	if err != nil {
		slog.Warn("capture: unreadable hook event", "err", err)
		return nil
	}
	if e.SessionID == "" {
		return nil
	}
	var rec store.RawRecord
	switch {
	case e.Prompt != "": // UserPromptSubmit
		rec = store.RawRecord{Role: "user", Text: e.Prompt}
	case e.LastAssistantMessage != "": // Stop
		rec = store.RawRecord{Role: "assistant", Text: e.LastAssistantMessage}
		if e.Effort != nil {
			if lvl, ok := e.Effort["level"].(string); ok {
				rec.Effort = lvl
			}
		}
	default:
		return nil
	}
	rec.TS = time.Now().UTC().Format(time.RFC3339)
	rec.Session = e.SessionID
	rec.Seq = st.NextSeq(e.SessionID)
	if err := st.AppendRaw(rec); err != nil {
		slog.Error("capture: append raw failed", "session", e.SessionID, "err", err)
	}
	return nil
}

// cmdSessionStart kicks the backlog sweep (self-healing for crashed/missed
// sessions). It NEVER injects the profile: witness is collect-only by design.
// The profile is pull-only — agents read it on demand via the MCP tools
// (get_facets / get_profile / search_observations) and humans via `witness
// profile` — so the SessionStart hook produces no additionalContext, only the
// worker kick.
func cmdSessionStart() error {
	st, err := store.Open()
	if err != nil {
		return nil
	}
	defer st.Close()
	// Kick the consumer iff there's actually work — distilling pending sessions or
	// a due review. The consumer (cmdWorker) is single-flight and drains
	// everything, so we don't spawn a process just to have it find nothing.
	cfg := st.LoadConfig()
	pending, _ := st.PendingSessions()
	if len(pending) > 0 || st.ReviewDue(cfg) {
		spawnDetached("worker")
	}
	return nil
}

// cmdSessionEnd spawns the worker for the just-ended session, detached.
//
// What fires SessionEnd (Claude Code `reason`, verified against the hooks docs):
//   - "clear"                       — user ran /clear
//   - "logout"                      — user logged out
//   - "prompt_input_exit"           — EOF/end of input in non-interactive (-p) mode
//   - "resume"                      — the prior session is suspended to be resumed
//   - "bypass_permissions_disabled" — bypass-permissions mode was turned off
//   - "other"                       — normal quit: Ctrl-C / Ctrl-D / closing the tab
//
// What does NOT fire it:
//   - compaction — that is PreCompact/PostCompact; the session continues with the
//     same id (we re-inject on SessionStart source=compact instead, not distill).
//   - hard kills (SIGKILL/crash/power loss) — the process dies before any hook runs;
//     the SessionStart backlog sweep recovers those next launch.
//
// We don't branch on the reason — any end means "distill what's new". We just kick
// the single-flight consumer, which drains ALL pending sessions (the watermark
// tells it what's new), so the specific session id isn't needed here.
// Distillation is delta-based, so resume→end→resume→end is safe.
func cmdSessionEnd() error {
	spawnDetached("worker")
	return nil
}

// spawnDetached re-execs this binary with the given args as a detached process,
// so hooks return instantly and the heavy work never blocks the session.
func spawnDetached(args ...string) {
	exe, err := os.Executable()
	if err != nil {
		return
	}
	cmd := exec.Command(exe, args...)
	cmd.Env = os.Environ()
	cmd.Stdin = nil
	cmd.Stdout = nil
	cmd.Stderr = nil
	// Put the worker in its own session so a SessionEnd-on-tab-close doesn't
	// SIGHUP it mid-distillation (the terminal signals only its own group).
	cmd.SysProcAttr = detachSysProcAttr()
	_ = cmd.Start() // fire and forget
	if cmd.Process != nil {
		_ = cmd.Process.Release()
	}
}

// cmdWorker is the single-flight background consumer. It holds one global lock for
// its whole run, drains EVERY pending session (delta-distilling each, once per
// run), then runs the reviewer if due. Triggers (session-start/end) just spawn
// this; if a consumer is already running, the new one no-ops immediately — no
// blocked-process pile-up, no daemon. The filesystem is the durable job queue;
// this lock elects the single consumer that drains it.
func cmdWorker(_ []string) error {
	st, err := store.Open()
	if err != nil {
		return err
	}
	defer st.Close()
	defer setupLogging(st)()

	unlock, ok := st.WorkerLock()
	if !ok {
		return nil // a consumer already holds the lock; our jobs are on disk for it
	}
	defer unlock()

	cfg := st.LoadConfig()
	lenses, err := activeLenses(st)
	if err != nil {
		slog.Error("load lenses", "err", err)
		return err
	}
	ctx := context.Background()

	// Embedder is heavy (~448MB); load it lazily and once, only if a session
	// actually needs mining. Review doesn't need it.
	var emb *embed.Embedder
	var embErr error
	getEmb := func() (*embed.Embedder, error) {
		if emb == nil && embErr == nil {
			emb, embErr = embed.New()
		}
		return emb, embErr
	}

	pending := func() []string { p, _ := st.PendingSessions(); return p }
	drainQueue(pending, func(session string) {
		e, err := getEmb()
		if err != nil {
			slog.Error("embedder", "err", err)
			return
		}
		w := &distill.Worker{Store: st, Embedder: e, Lenses: lenses, Config: cfg}
		if err := w.Process(ctx, session); err != nil {
			slog.Error("process session", "session", session, "err", err)
		}
	})

	// Review folded into the same single-flight pass (serialized under the lock,
	// so concurrent reviews can't clobber the facets). Due on the session-count
	// cap OR accumulated poignancy — whichever first. A successful review updates
	// the facets, so we regenerate the L4 narrative profile right after ("on
	// profile change"). The profile is purely derived: summarizing is best-effort
	// (log and move on), never failing the worker or blocking distillation.
	if st.ReviewDue(cfg) {
		r := &distill.Reviewer{Store: st, Lenses: lenses, Config: cfg}
		if err := r.Run(ctx, time.Now()); err != nil {
			slog.Error("review", "err", err)
		} else {
			slog.Info("review complete")
			regenerateProfile(ctx, st, cfg)
		}
	}
	return nil
}

// regenerateProfile refreshes the L4 narrative summaries from the current facets.
// Best-effort: any failure (missing prompts, a claude -p hiccup) is logged and
// swallowed, leaving the prior summaries in place — the profile is derived and
// non-critical, and must never break the worker.
func regenerateProfile(ctx context.Context, st *store.Store, cfg store.Config) {
	lensPrompt, unifiedPrompt, err := lens.LoadSummarizePrompts()
	if err != nil {
		slog.Warn("profile: summarizer prompts unavailable; skipping", "err", err)
		return
	}
	sm := &distill.Summarizer{Store: st, Config: cfg, LensPrompt: lensPrompt, UnifiedPrompt: unifiedPrompt}
	if err := sm.Summarize(ctx); err != nil {
		slog.Warn("profile: summary regeneration failed; keeping prior", "err", err)
		return
	}
	slog.Info("profile regenerated")
}

// setupLogging points slog at WITNESS_HOME/witness.log (JSON lines, append) and
// returns a closer. Each subcommand runs as its own process and configures its
// own default logger; failures that hooks would otherwise swallow land here.
func setupLogging(st *store.Store) func() {
	f, err := os.OpenFile(st.LogPath(), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return func() {}
	}
	slog.SetDefault(slog.New(slog.NewJSONHandler(f, &slog.HandlerOptions{Level: slog.LevelInfo})))
	return func() { _ = f.Close() }
}

func cmdMCP() error {
	st, err := store.Open()
	if err != nil {
		return err
	}
	defer st.Close()
	emb, err := embed.New()
	if err != nil {
		return err
	}
	return mcp.Serve(context.Background(), st, emb)
}

// cmdLens manages the central, global lens registry. Lenses are defined once and
// shared across every session (alongside the always-on "default" lens):
//
//	witness lens register <name> <file>   add/replace a lens definition
//	witness lens deregister <name>        remove a lens definition
//	witness lens enable <name>            run this lens on every session
//	witness lens disable <name>           stop running it
//	witness lens list                     show registered lenses + enabled state
func cmdLens(args []string) error {
	st, err := store.Open()
	if err != nil {
		return err
	}
	defer st.Close()
	if len(args) == 0 {
		return fmt.Errorf("usage: witness lens <register <name> <file>|deregister <name>|enable <name>|disable <name>|list>")
	}
	switch args[0] {
	case "register":
		if len(args) < 3 {
			return fmt.Errorf("usage: witness lens register <name> <file>")
		}
		if err := st.RegisterLens(args[1], args[2]); err != nil {
			return err
		}
		fmt.Printf("registered lens %q\n", args[1])
	case "deregister":
		if len(args) < 2 {
			return fmt.Errorf("usage: witness lens deregister <name>")
		}
		if err := st.DeregisterLens(args[1]); err != nil {
			return err
		}
		fmt.Printf("deregistered lens %q\n", args[1])
	case "enable":
		if len(args) < 2 || args[1] == "" {
			return fmt.Errorf("usage: witness lens enable <name>")
		}
		if !slices.Contains(st.RegisteredLenses(), args[1]) {
			return fmt.Errorf("lens %q is not registered (run: witness lens register %s <file>)", args[1], args[1])
		}
		if err := st.EnableLens(args[1]); err != nil {
			return err
		}
		fmt.Printf("enabled lens %q (runs on every session)\n", args[1])
	case "disable":
		if len(args) < 2 || args[1] == "" {
			return fmt.Errorf("usage: witness lens disable <name>")
		}
		if err := st.DisableLens(args[1]); err != nil {
			return err
		}
		fmt.Printf("disabled lens %q\n", args[1])
	case "list":
		enabled := st.LoadConfig().EnabledLenses
		reg := st.RegisteredLenses()
		if len(reg) == 0 {
			fmt.Println("no lenses registered")
			return nil
		}
		for _, name := range reg {
			state := "disabled"
			if slices.Contains(enabled, name) {
				state = "enabled"
			}
			fmt.Printf("%s\t%s\n", name, state)
		}
	default:
		return fmt.Errorf("unknown lens subcommand %q (want register|deregister|enable|disable|list)", args[0])
	}
	return nil
}

// activeLenses returns the default lens (always on) + every enabled, registered
// lens. All are global — they run on every session.
func activeLenses(st *store.Store) ([]*lens.Lens, error) {
	out := []*lens.Lens{}
	if p, err := lens.LoadDefault(); err == nil {
		out = append(out, p)
	} else {
		return nil, fmt.Errorf("load default lens: %w", err)
	}
	for _, name := range st.LoadConfig().EnabledLenses {
		l, err := lens.LoadRegistered(name, st.LensesDir())
		if err != nil {
			slog.Warn("enabled lens not loadable; skipping", "lens", name, "err", err)
			continue
		}
		out = append(out, l)
	}
	return out, nil
}

// cmdProfile prints the L4 narrative summary for a lens — the cross-lens unified
// portrait by default, or a specific lens (e.g. `witness profile math`). Raw
// markdown to stdout; it's already terminal-readable.
func cmdProfile(args []string) error {
	st, err := store.Open()
	if err != nil {
		return err
	}
	defer st.Close()
	lensName := "unified"
	if len(args) > 0 && strings.TrimSpace(args[0]) != "" {
		lensName = strings.TrimSpace(args[0])
	}
	md, ok, err := st.ReadProfile(lensName)
	if err != nil {
		return err
	}
	if !ok {
		fmt.Printf("no profile summary for %q yet — it's generated after the next background review.\n", lensName)
		return nil
	}
	fmt.Println(md)
	return nil
}

func cmdReview() error {
	st, err := store.Open()
	if err != nil {
		return err
	}
	defer st.Close()
	defer setupLogging(st)()
	cfg := st.LoadConfig()
	lenses, err := activeLenses(st)
	if err != nil {
		return err
	}
	r := &distill.Reviewer{Store: st, Lenses: lenses, Config: cfg}
	if err := r.Run(context.Background(), time.Now()); err != nil {
		return err
	}
	regenerateProfile(context.Background(), st, cfg)
	fmt.Println("review complete; profile regenerated")
	return nil
}

func cmdOpenCode(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: witness opencode <sync> [--wait] [session_id...]")
	}
	switch args[0] {
	case "sync":
		return cmdOpenCodeSync(args[1:], false)
	default:
		return fmt.Errorf("unknown opencode subcommand %q (want sync)", args[0])
	}
}

func cmdOpenCodeSync(args []string, quiet bool) error {
	wait := false
	var sessionIDs []string
	for _, arg := range args {
		switch arg {
		case "--wait":
			wait = true
		default:
			sessionIDs = append(sessionIDs, arg)
		}
	}
	st, err := store.Open()
	if err != nil {
		return err
	}
	defer st.Close()
	defer setupLogging(st)()

	unlock, ok := st.OpenCodeSyncLock()
	if !ok {
		return nil
	}

	stats, err := (&opencodeimport.Importer{Store: st}).Import(context.Background(), sessionIDs)
	unlock()
	if err != nil {
		return err
	}
	cfg := st.LoadConfig()
	pending, _ := st.PendingSessions()
	shouldRunWorker := stats.Records > 0 || len(pending) > 0 || st.ReviewDue(cfg)
	if shouldRunWorker && wait {
		if err := cmdWorker(nil); err != nil {
			return err
		}
	} else if shouldRunWorker {
		spawnDetached("worker")
	}
	if !quiet {
		fmt.Printf("opencode sync: imported %d raw record(s) from %d session(s)\n", stats.Records, stats.Sessions)
		if wait {
			fmt.Println("worker finished; run `witness review` to force L4 regeneration or `witness profile` to read existing summaries")
		} else {
			fmt.Println("worker kicked; run `witness doctor` or `witness profile` after distillation finishes")
		}
	}
	return nil
}

// cmdCleanup interactively reclaims bulky raw transcripts (L0) for sessions with
// no activity since a user-chosen cutoff (default 90 days). The derived archive —
// observations (L1) and the profile (facets, L2) — is KEPT; it's small and is the
// durable record. There is no automatic retention: pruning is a deliberate,
// confirmed user action, never a silent background delete.
func cmdCleanup() error {
	st, err := store.Open()
	if err != nil {
		return err
	}
	defer st.Close()

	in := bufio.NewReader(os.Stdin)
	fmt.Print("Delete raw messages from sessions with no activity in the last how many days? [90]: ")
	line, _ := in.ReadString('\n')
	days := 90
	if t := strings.TrimSpace(line); t != "" {
		n, err := strconv.Atoi(t)
		if err != nil || n <= 0 {
			return fmt.Errorf("not a positive number of days: %q", t)
		}
		days = n
	}
	cutoff := time.Now().AddDate(0, 0, -days).UTC().Format(time.RFC3339)

	sessions, records, err := st.RawPruneStats(cutoff)
	if err != nil {
		return err
	}
	if records == 0 {
		fmt.Printf("Nothing to clean: no sessions older than %d days.\n", days)
		return nil
	}
	fmt.Printf("\nThis will delete %d raw messages from %d session(s) idle since %s.\n",
		records, sessions, cutoff[:10])
	fmt.Println("Your observations and profile (L1/L2) are kept — only raw transcripts are removed.")
	fmt.Print("Proceed? [y/N]: ")
	conf, _ := in.ReadString('\n')
	if strings.ToLower(strings.TrimSpace(conf)) != "y" {
		fmt.Println("Aborted; nothing deleted.")
		return nil
	}

	ps, pr, err := st.PruneSessionsBefore(cutoff)
	if err != nil {
		return err
	}
	fmt.Printf("Cleaned: removed %d raw messages from %d session(s).\n", pr, ps)
	return nil
}

func cmdDoctor() error {
	fmt.Println("claude-witness doctor")
	st, err := store.Open()
	if err != nil {
		return err
	}
	defer st.Close()
	fmt.Printf("  data root: %s\n", st.Root)
	cfg := st.LoadConfig()
	fmt.Printf("  runner: %s | models: triage=%s distill=%s | review_every=%d poignancy=%d\n",
		cfg.Runner, cfg.TriageModel, cfg.DistillModel, cfg.ReviewEvery, cfg.ReviewPoignancy)
	if strings.EqualFold(cfg.Runner, "opencode") {
		fmt.Print("  opencode models: ")
		if err := distill.ValidateOpenCodeModels(context.Background(), cfg.TriageModel, cfg.DistillModel); err != nil {
			fmt.Printf("INVALID (%v)\n", err)
			return err
		}
		fmt.Println("OK")
	}

	stat := st.Stats()
	lastReview := stat.LastReview
	if lastReview == "" {
		lastReview = "never"
	}
	fmt.Printf("  archive: %d sessions, %d raw messages, %d observations, %d facets\n",
		stat.Sessions, stat.RawRecords, stat.Observations, stat.Facets)
	fmt.Printf("  queue: %d pending, %d backing off | last review: %s\n",
		stat.Pending, stat.BackedOff, lastReview)
	if stat.BackedOff > 0 {
		fmt.Println("  ⚠ sessions are backing off — mining is failing; check witness.log")
	}
	fmt.Println("  profile: collect-only (never injected); read via `witness profile`, MCP get_profile/get_facets, or query witness.db")

	fmt.Print("  embedder: ")
	emb, err := embed.New()
	if err != nil {
		fmt.Printf("UNAVAILABLE (%v)\n", err)
		return err
	}
	en, err := emb.Embed("I resolve uncertainty by running a cheap experiment.")
	if err != nil {
		return fmt.Errorf("embed EN: %w", err)
	}
	zh, err := emb.Embed("我通过做一个便宜的实验来解决不确定性。")
	if err != nil {
		return fmt.Errorf("embed ZH: %w", err)
	}
	un, _ := emb.Embed("The quarterly revenue report is due Friday.")
	fmt.Printf("OK (dim=%d)\n", len(en))
	fmt.Printf("  EN<->ZH cosine: %.4f | EN<->unrelated: %.4f (want first > second)\n",
		embed.Cosine(en, zh), embed.Cosine(en, un))
	return nil
}
