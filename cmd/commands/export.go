package commands

import (
	"fmt"
	"os"
	"path/filepath"
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
			"database snapshot plus what is NOT in the database — config.toml (enabled " +
			"lenses, runner, models), lenses/ (your lens definitions; prompt text lives " +
			"nowhere else, so an authored lens is otherwise unrecoverable), summarize/ (your " +
			"own portrait prompts, if you wrote any — the same holds), and profile/ (the " +
			"narrative, which is rebuilt from your facets whenever they change). Prefer --all " +
			"for backups.\n\n" +
			"Either form runs safely while the background worker is writing; no need to stop " +
			"it. Restoring needs no separate command: a --all directory has the same layout as " +
			"the data directory, so point WITNESS_HOME at it, or copy its contents into an " +
			"EMPTY data directory with witness stopped. If the machine already has an archive, " +
			"copying replaces it — use `witness merge <dir>` instead, which keeps both.",
		Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			return cmdExport(args[0], force, all, asJSON)
		},
	}
	c.Flags().BoolVarP(&force, "force", "f", false, "overwrite <path> if it already exists")
	c.Flags().BoolVarP(&all, "all", "a", false, "export the COMPLETE archive into <path> as a directory (db + config.toml + lenses/ + summarize/ + profile/)")
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
		abs, _ := filepath.Abs(path)
		fmt.Printf("%s exported the complete archive to %s\n", okGlyph(), tildePath(abs))
		for _, it := range items {
			switch {
			case it.Name == "witness.db":
				fmt.Println("    " + kvRow(it.Name, snapshotSummary(st, filepath.Join(path, it.Name)), ""))
			case it.Dir:
				fmt.Println("    " + kvRow(it.Name+"/", pluralize(it.Files, "file", "files"), ""))
			default:
				fmt.Println("    " + it.Name)
			}
		}
		fmt.Println(dim("  safe to sync — unlike the live data dir, whose WAL a syncer can corrupt"))
		fmt.Println(hint("restore", "WITNESS_HOME="+shellArg(abs)+" witness status"))
		fmt.Println(hint("into another", "witness merge "+shellArg(abs)))
		return nil
	}
	if err := st.Export(path, force); err != nil {
		return err
	}
	if asJSON {
		return emitJSON(map[string]string{"exported": path})
	}
	abs, _ := filepath.Abs(path)
	fmt.Printf("%s exported the database to %s\n", okGlyph(), tildePath(abs))
	fmt.Println("    " + kvRow(filepath.Base(path), snapshotSummary(st, path), ""))
	fmt.Println(dim("  safe to sync — unlike the live data dir, whose WAL a syncer can corrupt"))
	fmt.Println(dim("  the database only: config.toml, lenses/, summarize/ and profile/ are not in it"))
	dir := "<dir>" // a directory beside the snapshot when its name allows it, so the hint can be pasted
	if d := strings.TrimSuffix(abs, ".db"); d != abs {
		dir = shellArg(d)
	}
	fmt.Println(hint("everything", "witness export --all "+dir))
	return nil
}

// snapshotSummary says what a snapshot holds: its size, then the archive's counts at the moment it
// was taken. The output used to list file names only, so nothing showed the backup had the data.
func snapshotSummary(st *store.Store, db string) string {
	stat := st.Stats(activeLensNames(st))
	parts := []string{countOf(stat.Sessions, "session", "sessions"),
		countOf(stat.Observations, "observation", "observations"), countOf(stat.Facets, "active facet", "active facets")}
	if info, err := os.Stat(db); err == nil {
		parts = append([]string{humanBytes(info.Size())}, parts...)
	}
	return strings.Join(parts, " · ")
}
