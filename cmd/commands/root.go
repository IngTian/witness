package commands

import (
	"io"
	"os"
	"strings"

	// Register every platform with the registry (issue #21). These blank imports
	// are the DECLARED composition root: ForSession/ByName resolve only platforms
	// whose init() ran, and Default() panics if Claude is absent — so anchor both
	// here rather than relying on some command file happening to import them.
	_ "github.com/IngTian/witness/internal/platform/claude"
	_ "github.com/IngTian/witness/internal/platform/opencode"

	"github.com/spf13/cobra"
)

// Build-time variables, injected via -ldflags by the Makefile (and install.sh).
// Defaults render a runnable `go build` without ldflags as "dev".
var (
	version   = "dev"
	commit    = "none"
	buildTime = "unknown"
)

// Run is the CLI entry point used by cmd/witness/main.go. It owns the recursion
// guard (never act inside a witness-driven agent subprocess), cobra execution,
// and the exit-code contract: any RunE error exits 1.
func Run() int {
	// Belt-and-suspenders recursion guard (the shim also checks): never act when
	// running inside a witness-driven `claude -p`.
	if len(os.Args) > 1 && os.Getenv("WITNESS_WORKER") == "1" && os.Args[1] != "doctor" {
		_, _ = io.Copy(io.Discard, os.Stdin)
		return 0
	}
	if err := newRootCmd().Execute(); err != nil {
		reportError(err)
		return 1
	}
	return 0
}

func newRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:           "witness",
		Short:         "Capture, distill, and serve a person-centric growth archive.",
		Version:       version,
		SilenceUsage:  true,
		SilenceErrors: true,
		Long: strings.TrimSpace(`witness records raw assistant-session turns, distills them into observations and facets, and serves derived profiles through CLI and MCP.

The profile is collect-only and pull-only: witness never injects content into sessions. Humans read it with 'witness profile'; agents read it through MCP tools.`),
	}
	root.AddCommand(
		newVersionCmd(),
		newDoctorCmd(),
		newProfileCmd(),
		newFacetsCmd(),
		newObservationsCmd(),
		newReviewCmd(),
		newLensCmd(),
		newImportCmd(),
		newDistillCmd(),
		newCleanupCmd(),
		newExportCmd(),
		newInstallCmd(),
		newUninstallCmd(),
		newInternalCaptureCmd(),
		newInternalSessionStartCmd(),
		newInternalSessionEndCmd(),
		newInternalWorkerCmd(),
		newInternalWorkerWakeupCmd(),
		newInternalMCPCmd(),
	)
	return root
}
