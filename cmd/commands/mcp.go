package commands

import (
	"context"

	"github.com/IngTian/witness/internal/embed"
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
	emb, err := embed.New()
	if err != nil {
		return err
	}
	// Pull-based L4 (#100): get_profile regenerates a stale summary before serving it. The
	// callback keeps the runner/lens/WorkerLock machinery in cmd — internal/mcp stays a read
	// surface plus an embedder. ensureProfileFresh is lock-guarded and non-fatal: it serves
	// cached rather than queueing behind a running drain.
	return mcp.Serve(context.Background(), st, emb, version, func() error {
		_, err := ensureProfileFresh(st)
		return err
	})
}
