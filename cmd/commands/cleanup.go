package commands

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/IngTian/claude-witness/internal/store"
	"github.com/spf13/cobra"
)

func newCleanupCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "cleanup",
		Short: "Interactively prune old raw transcripts.",
		Long:  "Interactively delete old L0 raw messages for idle sessions while keeping derived observations, facets, and profiles. This is never automatic and asks for confirmation before deleting.",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			return cmdCleanup()
		},
	}
}

// cmdCleanup interactively reclaims bulky raw transcripts (L0) for sessions with
// no activity since a user-chosen cutoff (default 90 days). The derived archive —
// observations (L1) and the profile (facets, L2) — is KEPT; it's small and is the
// durable record. There is no automatic retention: pruning is a deliberate,
// confirmed user action, never a silent background delete.
func cmdCleanup() error {
	st, err := store.Open()
	if err != nil {
		return err
	}
	defer st.Close()

	in := bufio.NewReader(os.Stdin)
	fmt.Print("Delete raw messages from sessions with no activity in the last how many days? [90]: ")
	line, _ := in.ReadString('\n')
	days := 90
	if t := strings.TrimSpace(line); t != "" {
		n, err := strconv.Atoi(t)
		if err != nil || n <= 0 {
			return fmt.Errorf("not a positive number of days: %q", t)
		}
		days = n
	}
	cutoff := time.Now().AddDate(0, 0, -days).UTC().Format(time.RFC3339)

	sessions, records, err := st.RawPruneStats(cutoff)
	if err != nil {
		return err
	}
	if records == 0 {
		fmt.Printf("Nothing to clean: no sessions older than %d days.\n", days)
		return nil
	}
	fmt.Printf("\nThis will delete %d raw messages from %d session(s) idle since %s.\n",
		records, sessions, cutoff[:10])
	fmt.Println("Your observations and profile (L1/L2) are kept — only raw transcripts are removed.")
	fmt.Print("Proceed? [y/N]: ")
	conf, _ := in.ReadString('\n')
	if strings.ToLower(strings.TrimSpace(conf)) != "y" {
		fmt.Println("Aborted; nothing deleted.")
		return nil
	}

	ps, pr, err := st.PruneSessionsBefore(cutoff)
	if err != nil {
		return err
	}
	fmt.Printf("Cleaned: removed %d raw messages from %d session(s).\n", pr, ps)
	return nil
}
