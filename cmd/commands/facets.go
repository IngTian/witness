package commands

import (
	"fmt"
	"slices"
	"strings"

	"github.com/IngTian/witness/internal/store"
	"github.com/spf13/cobra"
)

func newFacetsCmd() *cobra.Command {
	var asJSON, all bool
	var expand string
	c := &cobra.Command{
		Use:     "facets [lens]",
		GroupID: groupRead,
		Short:   "Show the specific traits and patterns behind your profile.",
		Long: "Print the structured traits and patterns — organized by dimension — that witness currently tracks about you. " +
			"These are the building blocks that feed the narrative profile. Defaults to the default lens.\n\n" +
			"Shows the ACTIVE profile. Facets consolidated by `witness compact` are archived rather than deleted: " +
			"pass --all to list them too, or --expand <dimension/key> to unfold one active facet into the full " +
			"trajectory of facets that were merged into it.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			if expand != "" {
				return cmdFacetTrajectory(args, expand, asJSON)
			}
			return cmdFacets(args, asJSON, all)
		},
	}
	c.Flags().BoolVarP(&asJSON, "json", "j", false, "output as JSON")
	c.Flags().BoolVar(&all, "all", false, "include facets archived by compaction (marked with where they went)")
	c.Flags().StringVar(&expand, "expand", "", "unfold one facet (dimension/key) into everything merged into it")
	return c
}

func cmdFacets(args []string, asJSON, all bool) error {
	st, err := store.Open()
	if err != nil {
		return err
	}
	defer st.Close()
	lensName := store.LensDefault
	if len(args) > 0 && strings.TrimSpace(args[0]) != "" {
		lensName = strings.TrimSpace(args[0])
	}
	// The live profile by default: an archived facet is history, and listing it beside the
	// active ones would misrepresent what witness currently claims about the person.
	read := st.ReadActiveFacets
	if all {
		read = st.ReadFacets
	}
	facets, err := read()
	if err != nil {
		return err
	}
	if asJSON {
		filtered := []store.Facet{}
		for _, f := range facets {
			if f.Lens == lensName && f.Current() != nil {
				filtered = append(filtered, f)
			}
		}
		return emitJSON(facetsJSON{Lens: lensName, Facets: filtered})
	}
	// Whether this lens is registered AND enabled decides the right empty-state message:
	// since #44 slice 1a "default" is an ordinary, DELETABLE lens, so an absent lens (the
	// user removed it, or typo'd the name) must not be blamed on "the reviewer hasn't run
	// yet" — and a registered-but-DISABLED lens won't be helped by `witness review` either
	// (review only iterates the enabled set), so that hint must point at `lens enable`.
	registered := slices.Contains(st.RegisteredLenses(), lensName)
	enabled := slices.Contains(st.LoadConfig().EnabledLenses, lensName)
	// The listing shows CURRENT confidence (decayed to now); --json above stays the stored
	// record. docs/how-it-works.md promises this view moves on its own as evidence fails to
	// arrive, and before this it never did.
	hw, err := st.ObservationHighWater()
	if err != nil {
		return err
	}
	// whyEmpty is a closure, not a value: it costs two indexed queries and is only needed
	// on the empty path, which is the uncommon one.
	renderCurrentFacets(lensName, facets, hw, registered, enabled, func() store.EmptyReason { return st.WhyEmpty(lensName) })
	return nil
}

type facetsJSON struct {
	Lens   string        `json:"lens"`
	Facets []store.Facet `json:"facets"`
}

type trajectoryJSON struct {
	Lens      string        `json:"lens"`
	Facet     store.Facet   `json:"facet"`
	MergedIn  []store.Facet `json:"merged_in"`
	TotalKept int           `json:"total_versions_kept"`
}

// cmdFacetTrajectory answers "how did this facet get here" — the expansion half of a compact
// profile. Compaction keeps every absorbed facet with its full history and a link to the
// survivor, so a consolidated node is never a dead end: this unfolds it into the facets that
// were merged into it, and the ones merged into those.
func cmdFacetTrajectory(args []string, ref string, asJSON bool) error {
	st, err := store.Open()
	if err != nil {
		return err
	}
	defer st.Close()
	lensName := store.LensDefault
	if len(args) > 0 && strings.TrimSpace(args[0]) != "" {
		lensName = strings.TrimSpace(args[0])
	}
	dimension, key, ok := store.ParseFacetRef(strings.TrimSpace(ref))
	if !ok {
		return fmt.Errorf("--expand wants a facet as dimension/key (e.g. thinking/zooms_out), got %q", ref)
	}

	all, err := st.ReadFacets()
	if err != nil {
		return err
	}
	var target *store.Facet
	for i := range all {
		if all[i].Lens == lensName && all[i].Dimension == dimension && all[i].Key == key {
			target = &all[i]
			break
		}
	}
	if target == nil {
		return fmt.Errorf("no facet %s/%s in lens %q (see `witness facets %s --all`)", dimension, key, lensName, lensName)
	}
	tributaries, err := st.FacetTrajectory(lensName, dimension, key)
	if err != nil {
		return err
	}

	if asJSON {
		kept := len(target.Versions)
		for _, f := range tributaries {
			kept += len(f.Versions)
		}
		return emitJSON(trajectoryJSON{Lens: lensName, Facet: *target, MergedIn: tributaries, TotalKept: kept})
	}

	state := "active"
	if !target.Active() {
		state = "archived"
		if target.MergedInto != "" {
			state = "archived → merged into " + target.MergedInto
		}
	}
	fmt.Printf("%s/%s (%s lens, %s)\n\n", dimension, bold(key), lensName, state)
	if cur := target.Current(); cur != nil {
		fmt.Printf("  %s\n\n", cur.Value)
	}
	fmt.Printf("%s %s, %s of its own\n",
		label("history"), pluralize(len(tributaries), "facet merged in", "facets merged in"),
		pluralize(len(target.Versions), "version", "versions"))

	if len(tributaries) == 0 {
		fmt.Println(dim("\n  nothing was merged into this facet — its own versions are the whole story"))
		return nil
	}
	for _, f := range tributaries {
		fmt.Println()
		fmt.Printf("  %s %s/%s %s\n", dim("←"), f.Dimension, f.Key,
			dim(fmt.Sprintf("(archived %s, %s)", f.ArchivedAt, pluralize(len(f.Versions), "version", "versions"))))
		if cur := f.Current(); cur != nil {
			fmt.Printf("      %s\n", cur.Value)
		}
	}
	return nil
}

func renderCurrentFacets(lensName string, facets []store.Facet, decaySeq int64, registered, enabled bool, whyEmpty func() store.EmptyReason) {
	count := 0
	for _, f := range facets {
		if f.Lens == lensName && f.Current() != nil {
			count++
		}
	}
	if count == 0 {
		// Three distinct empty states, each with the ACCURATE next step — the deletable
		// default (#102) makes "absent" and "disabled" normal, not a stuck reviewer:
		switch {
		case !registered:
			fmt.Printf("No lens %q is registered, so it has no facets (see `witness lens list`; restore the built-in one with `witness lens load-default`).\n", lensName)
		case !enabled:
			// `witness worker review` only iterates the ENABLED set, so it would no-op here —
			// point at enable, not review.
			fmt.Printf("Lens %q is registered but disabled, so it has no facets — enable it with `witness lens enable %s`, then distill.\n", lensName, lensName)
		default:
			// Registered AND enabled, so the lens itself is fine — the question is which
			// layer is missing. "The reviewer runs after enough observations accumulate" is
			// only true when observations EXIST; telling someone with no capture wired to
			// wait for the reviewer describes a fix that will never arrive (#166).
			switch whyEmpty() {
			case store.NoCapture:
				fmt.Printf("No facets for lens %q — and this archive holds no raw turns at all, so nothing is capturing.\n", lensName)
				fmt.Println("Wire an editor: `witness wire claude`, or add the plugin to opencode.json for OpenCode. Waiting will not help.")
			case store.NoObservations:
				fmt.Printf("No facets for lens %q — capture is working, but nothing has been mined into it yet.\n", lensName)
				fmt.Printf("Mine it over existing history with `witness lens backfill %s`.\n", lensName)
			default:
				fmt.Printf("No facets for lens %q yet — the reviewer runs after enough observations accumulate; force one now with `witness worker review`.\n", lensName)
			}
		}
		return
	}
	fmt.Printf("Growth facets (%s lens) - %d\n\n", lensName, count)
	for _, f := range facets {
		if f.Lens != lensName {
			continue
		}
		cur := f.Current()
		if cur == nil {
			continue
		}
		fmt.Printf("- %s/%s (confidence %.2f)\n", f.Dimension, f.Key, cur.DecayedConfidence(decaySeq))
		// Only reachable under --all. An archived facet listed without saying so would read as
		// a live claim about the person, which is exactly what compaction decided it is not.
		if !f.Active() {
			if f.MergedInto != "" {
				fmt.Printf("  %s %s\n", dim("archived — merged into"), f.MergedInto)
			} else {
				fmt.Printf("  %s\n", dim("archived — retired"))
			}
		}
		fmt.Printf("  %s\n", cur.Value)
		if f.LastSeen != "" {
			fmt.Printf("  last_seen: %s\n", f.LastSeen)
		}
		if len(cur.BecauseOf) > 0 {
			fmt.Printf("  because_of: %s\n", strings.Join(cur.BecauseOf, ", "))
		}
	}
}
