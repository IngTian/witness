package commands

import (
	"fmt"
	"strings"

	"github.com/IngTian/witness/internal/store"
	"github.com/spf13/cobra"
)

func newProfileCmd() *cobra.Command {
	var asJSON bool
	c := &cobra.Command{
		Use:   "profile [lens]",
		Short: "Print the narrative profile.",
		Long: "Print the narrative profile (markdown).\n\n" +
			"With NO argument, prints the 'unified' profile — the cross-lens portrait that blends every enabled lens into one whole-person view (generated once 2+ lenses have accumulated facets). 'unified' is an aggregate VIEW, not a lens.\n\n" +
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
	md, ok, err := st.ReadProfile(lensName)
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
	if !ok {
		fmt.Printf("no profile summary for %q yet — it's generated after the next background review.\n", lensName)
		return nil
	}
	// Decorative freshness header only; the profile body (md) is LLM-authored
	// markdown and printed verbatim.
	fmt.Printf("%s %s\n", dim("profile:"), cyan(lensName))
	fmt.Printf("  %s %s\n", label("distilled"), fresh.DistilledThrough)
	fmt.Printf("  %s %s\n", label("raw"), fresh.RawThrough)
	pendingText := fmt.Sprintf("%d sessions", fresh.Pending)
	if fresh.Pending > 0 {
		pendingText = yellow(pendingText + " awaiting distillation")
	}
	fmt.Printf("  %s %s\n", label("pending"), pendingText)
	fmt.Println(dim(strings.Repeat("─", 60)))
	fmt.Println(md)
	return nil
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
