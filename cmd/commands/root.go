package commands

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"runtime/debug"
	"strings"

	// Register every platform with the registry (issue #21). These blank imports
	// are the DECLARED composition root: ForSession/ByName resolve only platforms
	// whose init() ran, and Default() panics if Claude is absent — so anchor both
	// here rather than relying on some command file happening to import them.
	_ "github.com/IngTian/witness/internal/platform/claude"
	_ "github.com/IngTian/witness/internal/platform/codex"
	_ "github.com/IngTian/witness/internal/platform/file"
	_ "github.com/IngTian/witness/internal/platform/opencode"

	"github.com/spf13/cobra"
)

const (
	groupRead   = "read"
	groupLenses = "lenses"
	groupConfig = "config"
	groupSetup  = "setup"
	groupAdmin  = "admin"
)

// Build-time variables, injected via -ldflags by the Makefile. install.sh, `go build` and
// `go install` pass none, so init fills them from the VCS stamp Go writes into every build;
// they stay "dev" only when there is nothing to read (`go test`, a build outside a checkout).
var (
	version   = "dev"
	commit    = "none"
	buildTime = "unknown"
)

func init() {
	if version != "dev" {
		return // ldflags win: a release build says exactly what the Makefile stamped
	}
	if bi, ok := debug.ReadBuildInfo(); ok {
		version, commit, buildTime = buildStamp(bi)
	}
}

// buildStamp reads the version Go derives from the tag/commit (already "+dirty" when the tree
// was), the short revision, and vcs.time. vcs.time is the COMMIT time, not the build time — the
// closest thing a plain build records, and enough to tell two binaries apart.
func buildStamp(bi *debug.BuildInfo) (v, c, at string) {
	v, c, at = "dev", "none", "unknown"
	if mv := bi.Main.Version; mv != "" && mv != "(devel)" {
		v = mv
	}
	dirty := false
	for _, s := range bi.Settings {
		switch s.Key {
		case "vcs.revision":
			c = s.Value[:min(7, len(s.Value))]
		case "vcs.time":
			at = s.Value
		case "vcs.modified":
			dirty = s.Value == "true"
		}
	}
	if dirty && c != "none" {
		c += "+dirty"
	}
	return v, c, at
}

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
	// Until a command opens witness.log (setupLogging), slog must go nowhere. Its stdlib default is
	// stderr, so the first-run "seeded the built-in default lens …" INFO line — internal issue number
	// and all — was the first thing a new user saw, and the capture hooks printed it into the editor
	// on their very first event. Anything a person must see is printed through the style helpers.
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := newRootCmd().Execute(); err != nil {
		reportError(err)
		return 1
	}
	return 0
}

func newRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:   "witness",
		Short: "Capture, distill, and serve a person-centric growth archive.",
		// commit + buildTime are ldflags-injected by the Makefile on EVERY build, and were
		// previously never displayed anywhere — staticcheck flagged both as unused, correctly. The
		// values were being computed and thrown away.
		//
		// Surfaced here rather than deleted because identifying the exact binary is the first step
		// of any bug report, and a released tag is not enough on its own: the stale-binary incident
		// (a bin/ that predated five releases while reviews silently did nothing) was diagnosable
		// only by build metadata. `witness --version` now answers "which build is this" without
		// requiring the user to check file mtimes.
		Version:       fmt.Sprintf("%s (commit %s, built %s)", version, commit, buildTime),
		SilenceUsage:  true,
		SilenceErrors: true,
		Long: strings.TrimSpace(`witness records raw assistant-session turns, distills them into observations and facets, and serves derived profiles through CLI and MCP.

The profile is collect-only and pull-only: witness never injects content into sessions. Humans read it with 'witness profile'; agents read it through MCP tools.`),
	}
	root.AddGroup(
		&cobra.Group{ID: groupRead, Title: "Read your archive:"},
		&cobra.Group{ID: groupLenses, Title: "Lenses:"},
		&cobra.Group{ID: groupConfig, Title: "Configure:"},
		&cobra.Group{ID: groupSetup, Title: "Setup:"},
		&cobra.Group{ID: groupAdmin, Title: "Maintenance:"},
	)
	root.SetHelpCommandGroupID(groupAdmin)
	root.SetCompletionCommandGroupID(groupAdmin)
	root.AddCommand(
		newProfileCmd(),
		newViewCmd(),
		newStatusCmd(),
		newFacetsCmd(),
		newObservationsCmd(),
		newLensCmd(),
		newConfigCmd(),
		newIngestCmd(),
		newInstallCmd(),
		newWireCmd(),
		newUnwireCmd(),
		newDoctorCmd(),
		newExportCmd(),
		newMergeCmd(),
		newCompactCmd(),
		newCleanupCmd(),
		newWorkerCmd(),
		newImportCmd(),
		newInternalCaptureCmd(),
		newInternalSessionStartCmd(),
		newInternalSessionEndCmd(),
		newInternalWorkerCmd(),
		newInternalWorkerKickCmd(),
		newInternalWorkerWakeupCmd(),
		newInternalMCPCmd(),
	)
	return root
}
