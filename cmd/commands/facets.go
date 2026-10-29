package commands

import (
	"fmt"
	"slices"
	"strings"

	"github.com/IngTian/witness/internal/store"
	"github.com/spf13/cobra"
)

func newFacetsCmd() *cobra.Command {
	var asJSON bool
	c := &cobra.Command{
		Use:     "facets [lens]",
		GroupID: groupRead,
		Short:   "Show the specific traits and patterns behind your profile.",
		Long:    "Print the structured traits and patterns — organized by dimension — that witness currently tracks about you. These are the building blocks that feed the narrative profile. Defaults to the default lens.",
		Args:    cobra.MaximumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			return cmdFacets(args, asJSON)
		},
	}
	c.Flags().BoolVarP(&asJSON, "json", "j", false, "output as JSON")
	return c
}

func cmdFacets(args []string, asJSON bool) error {
	st, err := store.Open()
	if err != nil {
		return err
	}
	defer st.Close()
	lensName := store.LensDefault
	if len(args) > 0 && strings.TrimSpace(args[0]) != "" {
		lensName = strings.TrimSpace(args[0])
	}
	facets, err := st.ReadFacets()
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
	// whyEmpty is a closure, not a value: it costs two indexed queries and is only needed
	// on the empty path, which is the uncommon one.
	renderCurrentFacets(lensName, facets, registered, enabled, func() store.EmptyReason { return st.WhyEmpty(lensName) })
	return nil
}

type facetsJSON struct {
	Lens   string        `json:"lens"`
	Facets []store.Facet `json:"facets"`
}

func renderCurrentFacets(lensName string, facets []store.Facet, registered, enabled bool, whyEmpty func() store.EmptyReason) {
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
		fmt.Printf("- %s/%s (confidence %.2f)\n", f.Dimension, f.Key, cur.Confidence)
		fmt.Printf("  %s\n", cur.Value)
		if f.LastSeen != "" {
			fmt.Printf("  last_seen: %s\n", f.LastSeen)
		}
		if len(cur.BecauseOf) > 0 {
			fmt.Printf("  because_of: %s\n", strings.Join(cur.BecauseOf, ", "))
		}
	}
}
