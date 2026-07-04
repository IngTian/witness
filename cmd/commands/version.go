package commands

import (
	"fmt"
	"runtime"

	"github.com/spf13/cobra"
)

func newVersionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print witness version and build info.",
		Long:  "Print the witness version, git commit, build time, and the Go runtime (OS/arch/Go version). Build info is injected via ldflags at compile time; a bare 'go build' reports 'dev'.",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			fmt.Printf("witness %s\n", version)
			fmt.Printf("  commit:     %s\n", commit)
			fmt.Printf("  built:      %s\n", buildTime)
			fmt.Printf("  runtime:    %s/%s\n", runtime.GOOS, runtime.GOARCH)
			fmt.Printf("  go version: %s\n", runtime.Version())
			return nil
		},
	}
}
