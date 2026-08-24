package commands

import (
	"fmt"
	"strings"

	"github.com/IngTian/witness/internal/store"
	"github.com/spf13/cobra"
)

func newExportCmd() *cobra.Command {
	var force bool
	var asJSON bool
	var all bool
	c := &cobra.Command{
		Use:     "export <path>",
		GroupID: groupAdmin,
		Short:   "Write a consistent snapshot of the archive.",
		Long: "Write a consistent snapshot of the archive.\n\n" +
			"By default <path> is a FILE and receives a single-file SQLite snapshot of the " +
			"database (via VACUUM INTO): the WAL is folded into one plain .db with no " +
			"-wal/-shm sidecars, so it is safe to copy or point a cloud syncer " +
			"(iCloud/Dropbox/Drive) at it — unlike the live data directory, whose WAL a syncer " +
			"can corrupt.\n\n" +
			"With --all, <path> is a DIRECTORY and receives a complete backup: that same " +
			"database snapshot plus the three things that are NOT in the database — " +
			"config.toml (enabled lenses, runner, models), lenses/ (your lens definitions; " +
			"prompt text lives nowhere else, so an authored lens is otherwise unrecoverable), " +
			"and profile/ (the narrative, including a hand-edited unified.md). Prefer --all " +
			"for backups.\n\n" +
			"Either form runs safely while the background worker is writing; no need to stop " +
			"it. Restoring needs no separate command: a --all directory has the same layout as " +
			"the data directory, so point WITNESS_HOME at it, or copy its contents into your " +
			"data directory with witness stopped. A database-only snapshot restores the same " +
			"way as witness.db.",
		Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			return cmdExport(args[0], force, all, asJSON)
		},
	}
	c.Flags().BoolVarP(&force, "force", "f", false, "overwrite <path> if it already exists")
	c.Flags().BoolVarP(&all, "all", "a", false, "export the COMPLETE archive into <path> as a directory (db + config.toml + lenses/ + profile/)")
	c.Flags().BoolVarP(&asJSON, "json", "j", false, "output as JSON")
	return c
}

// exportAllJSON is the --all report: what was actually captured, so a scripted backup
// can assert on it (a missing "lenses" entry means there were none to copy).
type exportAllJSON struct {
	Exported string             `json:"exported"`
	All      bool               `json:"all"`
	Items    []store.ExportItem `json:"items"`
}

func cmdExport(path string, force, all, asJSON bool) error {
	path = strings.TrimSpace(path)
	// A .db destination with --all would produce a DIRECTORY named foo.db, which reads as
	// a corrupt database to every tool the user might reach for next. Catch the mixed-up
	// invocation rather than creating the confusing artifact.
	if all && strings.HasSuffix(strings.ToLower(path), ".db") {
		return fmt.Errorf("export --all writes a DIRECTORY, but %s looks like a database file; drop the .db suffix (or omit --all for a database-only snapshot)", path)
	}
	st, err := store.Open()
	if err != nil {
		return err
	}
	defer st.Close()
	if all {
		items, err := st.ExportAll(path, force)
		if err != nil {
			return err
		}
		if asJSON {
			return emitJSON(exportAllJSON{Exported: path, All: true, Items: items})
		}
		fmt.Printf("exported complete archive to %s\n", path)
		for _, it := range items {
			name := it.Name
			if it.Dir {
				fmt.Printf("  %-14s %d files\n", name+"/", it.Files)
				continue
			}
			fmt.Printf("  %s\n", name)
		}
		fmt.Println("safe to sync this directory (cloud/backup); do NOT sync the live data dir (WAL corruption).")
		fmt.Println("to restore: set WITNESS_HOME to this directory, or copy its contents into your data dir with witness stopped.")
		return nil
	}
	if err := st.Export(path, force); err != nil {
		return err
	}
	if asJSON {
		return emitJSON(map[string]string{"exported": path})
	}
	fmt.Printf("exported archive snapshot to %s\n", path)
	fmt.Println("safe to sync this file (cloud/backup); do NOT sync the live data dir (WAL corruption).")
	fmt.Println("note: the database only — use --all to include config.toml, lenses/ and profile/.")
	return nil
}
