package commands

import (
	"fmt"
	"os"
	"slices"
	"strings"

	"github.com/IngTian/witness/internal/store"
	"github.com/spf13/cobra"
)

func newProfileCmd() *cobra.Command {
	var asJSON bool
	c := &cobra.Command{
		Use:     "profile [lens]",
		GroupID: groupRead,
		Short:   "Read your narrative growth summary.",
		Long: "Print the narrative profile (markdown).\n\n" +
			"With NO argument, prints the 'unified' profile — the cross-lens portrait that blends every enabled lens into one whole-person view. 'unified' is an aggregate VIEW, not a lens.\n\n" +
			"Pass a lens name for that lens's own profile: `witness profile default` is the built-in person-growth lens's narrative (distinct from the unified blend); `witness profile <name>` any other enabled lens. See `witness lens list` for what's enabled.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			return cmdProfile(args, asJSON)
		},
	}
	c.Flags().BoolVarP(&asJSON, "json", "j", false, "output as JSON")
	return c
}

// cmdProfile prints the L4 narrative summary for a lens — the cross-lens unified
// portrait by default, or a specific lens (e.g. `witness profile math`). Raw
// markdown to stdout; it's already terminal-readable. In --json mode, emits an
// object with lens / markdown / freshness fields (markdown is "" when no summary
// has been generated yet, so consumers can distinguish empty-from-pending).
func cmdProfile(args []string, asJSON bool) error {
	st, err := store.Open()
	if err != nil {
		return err
	}
	defer st.Close()
	lensName := store.ProfileUnified
	if len(args) > 0 && strings.TrimSpace(args[0]) != "" {
		lensName = strings.TrimSpace(args[0])
	}
	// Regenerate on READ if stale (#100). Measured: a cached read is ~0.02s, a regeneration
	// ~13s on a real `claude -p`. Non-fatal by design — if the profile cannot be rebuilt right
	// now (no runner, a provider outage, a drain holding the lock) we still serve whatever is on
	// disk, because a stale narrative beats no narrative. The notice goes to stderr so `witness
	// profile > file` still captures only the markdown.
	if _, err := ensureProfileFresh(st); err != nil && !asJSON {
		// Headline + cause on its own line: the cause is a runner error of any length, and it is
		// the whole answer to "why is this stale" (rule: failures show their cause on screen).
		fmt.Fprintln(os.Stderr, errLine("!", "[!]", ansiYellow, "could not refresh the profile — showing the last one"))
		fmt.Fprintln(os.Stderr, "  "+styledErr(ansiDim, err.Error()))
	}
	// A profile is written under the lens's MINED name, which a registered lens may override.
	md, ok, err := st.ReadProfile(lensDataName(st, lensName))
	if err != nil {
		return err
	}
	stat := st.Stats(activeLensNames(st))
	fresh := profileFreshness{
		DistilledThrough: valueOrNever(st.LastDistilledRawTS()),
		RawThrough:       valueOrNever(st.LastRawTS()),
		Pending:          stat.Pending,
	}
	if asJSON {
		out := profileJSON{Lens: lensName, Freshness: fresh}
		if ok {
			out.Markdown = md
		}
		return emitJSON(out)
	}
	fmt.Println(title("profile", lensName, ""))
	fmt.Println()
	if !ok {
		renderProfileEmpty(st, lensName)
		return nil
	}
	// Decorative freshness header only. The body is LLM-authored markdown: verbatim in a pipe or
	// a file, and only on a terminal are its headings and **bold** rendered instead of printed.
	fmt.Println("  " + kvRow("distilled", shortTime(fresh.DistilledThrough), ""))
	fmt.Println("  " + kvRow("raw", shortTime(fresh.RawThrough), ""))
	if fresh.Pending > 0 {
		fmt.Println("  " + kvRow("pending", yellow(countNoun(fresh.Pending, "session", "sessions")), "not in this profile yet"))
	} else {
		fmt.Println("  " + kvRow("pending", "0 sessions", ""))
	}
	fmt.Println("  " + rule(60))
	fmt.Println()
	for _, line := range strings.Split(md, "\n") {
		fmt.Println(markdownLine(line))
	}
	return nil
}

// renderProfileEmpty names the layer a missing profile is waiting on, and the command that fills
// it. "It's generated after the next background review" was the only answer before — on an
// archive that captures nothing, that review never comes.
func renderProfileEmpty(st *store.Store, lensName string) {
	unified := lensName == store.ProfileUnified
	switch {
	case !unified && !slices.Contains(st.RegisteredLenses(), lensName):
		fmt.Printf("  %s no lens %q is registered\n", skipGlyph(), lensName)
		fmt.Println(hint("see lenses", "witness lens list"))
	case st.LastRawTS() == "":
		fmt.Printf("  %s no profile yet — nothing is capturing (the archive holds no raw turns)\n", skipGlyph())
		fmt.Println(hint("wire editor", wireCommand()))
	case !unified && !slices.Contains(st.LoadConfig().EnabledLenses, lensName):
		fmt.Printf("  %s lens %q is disabled, so it has no profile\n", skipGlyph(), lensName)
		fmt.Println(hint("enable it", "witness lens enable "+lensName))
	case unified:
		// The portrait is written on read from the enabled lenses' facets.
		fmt.Printf("  %s no profile yet — it is written from the enabled lenses' facets, and there are none\n", skipGlyph())
		fmt.Println(hint("see why", "witness facets"))
	default:
		fmt.Printf("  %s no profile for %q yet — it is written from the lens's facets, and there are none\n", skipGlyph(), lensName)
		fmt.Println(hint("see why", "witness facets "+lensName))
	}
}

type profileJSON struct {
	Lens      string           `json:"lens"`
	Markdown  string           `json:"markdown"`
	Freshness profileFreshness `json:"freshness"`
}

type profileFreshness struct {
	DistilledThrough string `json:"distilled_through"`
	RawThrough       string `json:"raw_through"`
	Pending          int    `json:"pending"`
}
