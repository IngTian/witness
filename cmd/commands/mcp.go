package commands

import (
	"context"

	"github.com/IngTian/witness/internal/embed"
	"github.com/IngTian/witness/internal/lens"
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
	if lazyModel() {
		defer setupLogging(st)() // the background download is otherwise silent
		return mcp.Serve(context.Background(), st, startLazyEmbedder(), mcpOptions(st))
	}
	emb, err := embed.New()
	if err != nil {
		return err
	}
	return mcp.Serve(context.Background(), st, emb, mcpOptions(st))
}

// mcpOptions is everything the MCP server needs from cmd, so internal/mcp stays a read surface
// plus an embedder.
func mcpOptions(st *store.Store) mcp.Options {
	return mcp.Options{
		Version: version,
		// Pull-based L4 (#100): get_profile regenerates a stale summary before serving it.
		// ensureProfileFresh is lock-guarded and non-fatal: it serves cached rather than queueing
		// behind a running drain.
		Refresh: func() error {
			_, err := ensureProfileFresh(st)
			return err
		},
		// Agents name a lens the way every command does; its rows carry the mined name.
		DataName: func(name string) string { return lens.DataName(st, name) },
		Wire:     wireCommand(),
	}
}
