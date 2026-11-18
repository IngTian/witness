package commands

import (
	"context"
	"strings"

	"github.com/IngTian/witness/internal/mcp"
	"github.com/IngTian/witness/internal/store"
	"github.com/spf13/cobra"
)

func newInternalMCPCmd() *cobra.Command {
	return &cobra.Command{Use: "mcp", Hidden: true, Args: cobra.NoArgs, RunE: func(_ *cobra.Command, _ []string) error { return cmdMCP() }}
}

func cmdMCP() error {
	st, err := store.Open()
	if err != nil {
		return err
	}
	defer st.Close()
	fetch := lazyModel()
	if fetch {
		defer setupLogging(st)() // the background download is otherwise silent
	}
	return mcp.Serve(context.Background(), st, mcpEmbedder(fetch), mcpOptions(st))
}

// mcpOptions is everything the MCP server needs from cmd, so internal/mcp stays a read surface
// plus an embedder.
func mcpOptions(st *store.Store) mcp.Options {
	return mcp.Options{
		Version: version,
		// MCP never regenerates; the closure is Summarize's own skip test. It takes no lock,
		// opens no runner and writes nothing; when staleness cannot be computed the portrait
		// reads as current, as in `witness view`.
		ProfileStale: func(name string) bool {
			sm, err := profileSummarizer(st)
			if err != nil || sm == nil {
				return false
			}
			stale, err := sm.Stale()
			if err != nil {
				return false
			}
			// No lens has facets when Stale holds only unified: Summarize then deletes unified.md,
			// though Stale calls it current (it never calls the model there).
			noFacets := len(stale) == 1
			s, ok := stale[name]
			if !ok {
				// macOS and Windows fold case, so get_profile("Math") is served math.md; the lookup
				// must fold the same way or that current portrait reads as an orphan Summarize reaps.
				// Registration refuses names that differ only in case, so at most one key matches.
				for k, v := range stale {
					if strings.EqualFold(k, name) {
						s, ok = v, true
						break
					}
				}
			}
			if ok && !noFacets {
				return s
			}
			// Absent from Stale = no facets: a portrait still on disk is one Summarize reaps (#100).
			_, onDisk, _ := st.ReadProfile(name)
			return onDisk
		},
		Wire: wireCommand(),
	}
}
