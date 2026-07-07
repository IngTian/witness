package commands

import (
	"github.com/IngTian/witness/internal/store"
	"github.com/spf13/cobra"
)

func newInternalSessionStartCmd() *cobra.Command {
	return &cobra.Command{Use: "session-start", Hidden: true, Args: cobra.NoArgs, RunE: func(_ *cobra.Command, _ []string) error { return cmdSessionStart() }}
}

func newInternalSessionEndCmd() *cobra.Command {
	return &cobra.Command{Use: "session-end", Hidden: true, Args: cobra.NoArgs, RunE: func(_ *cobra.Command, _ []string) error { return cmdSessionEnd() }}
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
