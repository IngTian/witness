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
	c := &cobra.Command{
		Use:   "export <path>",
		Short: "Write a consistent single-file snapshot of the archive.",
		Long: "Write a consistent, single-file SQLite snapshot of the archive to <path> " +
			"(via VACUUM INTO). The snapshot folds the write-ahead log into one plain .db " +
			"file with no -wal/-shm sidecars, so it is safe to copy or point a cloud syncer " +
			"(iCloud/Dropbox/Drive) at it — unlike the live data directory, whose WAL a syncer " +
			"can corrupt. Runs safely even while the background worker is writing; no need to " +
			"stop it. The snapshot is itself a normal witness.db: to restore, stop witness and " +
			"copy it into your data dir as witness.db (or set WITNESS_HOME to its folder).",
		Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			return cmdExport(args[0], force, asJSON)
		},
	}
	c.Flags().BoolVarP(&force, "force", "f", false, "overwrite <path> if it already exists")
	c.Flags().BoolVarP(&asJSON, "json", "j", false, "output as JSON")
	return c
}

func cmdExport(path string, force, asJSON bool) error {
	path = strings.TrimSpace(path)
	st, err := store.Open()
	if err != nil {
		return err
	}
	defer st.Close()
	if err := st.Export(path, force); err != nil {
		return err
	}
	if asJSON {
		return emitJSON(map[string]string{"exported": path})
	}
	fmt.Printf("exported archive snapshot to %s\n", path)
	fmt.Println("safe to sync this file (cloud/backup); do NOT sync the live data dir (WAL corruption).")
	return nil
}
