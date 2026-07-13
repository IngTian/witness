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
	return mcp.Serve(context.Background(), st, emb, version)
}
