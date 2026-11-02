package commands

import (
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/mattn/go-isatty"

	"github.com/IngTian/witness/internal/store"
	"github.com/spf13/cobra"
)

func newCleanupCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "cleanup",
		GroupID: groupAdmin,
		Short:   "Interactively prune old raw transcripts.",
		Long:    "Interactively delete old raw messages for idle sessions while keeping derived observations, facets, and profiles. This is never automatic and asks for confirmation before deleting.",
		Args:    cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			return cmdCleanup(os.Stdin, askYesNo)
		},
	}
}

// cmdCleanup interactively reclaims bulky raw transcripts (L0) for sessions with
// no activity since a user-chosen cutoff (default 90 days). The derived archive —
// observations (L1) and the profile (facets, L2) — is KEPT; it's small and is the
// durable record. There is no automatic retention: pruning is a deliberate,
// confirmed user action, never a silent background delete.
//
// The confirmation goes through ask (askYesNo in production) like every other y/N: this was the one
// destructive prompt with its own reader, and a piped or Ctrl-D answer left "Proceed? [y/N]:
// Aborted; nothing deleted." on one line.
func cmdCleanup(in io.Reader, ask func(prompt string) bool) error {
	st, err := store.Open()
	if err != nil {
		return err
	}
	defer st.Close()

	fmt.Print(bold("Prune raw transcripts idle for how many days?") + " " + dim("[90]") + ": ")
	line := readLine(in)
	if f, ok := in.(*os.File); !ok || !isatty.IsTerminal(f.Fd()) {
		fmt.Println(line) // a terminal echoes what was typed; piped input would leave the line open
	}
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
		fmt.Printf("%s nothing to clean — no session idle for %s\n", skipGlyph(), pluralize(days, "day", "days"))
		return nil
	}
	fmt.Println()
	fmt.Println(header("cleanup") + dim(" — raw transcripts only"))
	fmt.Println("  " + kvRow("idle since", cutoff[:10], pluralize(days, "day", "days")))
	fmt.Println("  " + kvRow("deletes", yellow(countOf(records, "raw message", "raw messages"))+" from "+countOf(sessions, "session", "sessions"), ""))
	fmt.Println("  " + kvRow("keeps", "observations · facets · profile", ""))
	fmt.Println()
	if !ask(bold("Delete them?") + dim(" This cannot be undone.") + " [y/N]: ") {
		fmt.Println(dim("not deleted — nothing written"))
		return nil
	}

	ps, pr, err := st.PruneSessionsBefore(cutoff)
	if err != nil {
		return err
	}
	fmt.Printf("%s removed %s from %s\n", okGlyph(), countOf(pr, "raw message", "raw messages"), countOf(ps, "session", "sessions"))
	return nil
}

// readLine reads one line, a byte at a time, so nothing past the newline is buffered away from the
// next reader: askYesNo reads the confirmation from the same stdin, and a bufio.Reader here would
// swallow a piped "y" along with the day count.
func readLine(in io.Reader) string {
	var b strings.Builder
	buf := make([]byte, 1)
	for {
		n, err := in.Read(buf)
		if n == 1 {
			if buf[0] == '\n' {
				break
			}
			b.WriteByte(buf[0])
		}
		if err != nil {
			break
		}
	}
	return strings.TrimRight(b.String(), "\r")
}
