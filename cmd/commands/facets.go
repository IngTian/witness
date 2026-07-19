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
		Use:   "facets [lens]",
		Short: "Print current structured facets.",
		Long:  "Print the current L2 structured facets for a lens. This is the CLI equivalent of the MCP get_facets tool. Defaults to the default lens.",
		Args:  cobra.MaximumNArgs(1),
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
	renderCurrentFacets(lensName, facets, registered, enabled)
	return nil
}

type facetsJSON struct {
	Lens   string        `json:"lens"`
	Facets []store.Facet `json:"facets"`
}

func renderCurrentFacets(lensName string, facets []store.Facet, registered, enabled bool) {
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
			// `witness review` only iterates the ENABLED set, so it would no-op here —
			// point at enable, not review.
			fmt.Printf("Lens %q is registered but disabled, so it has no facets — enable it with `witness lens enable %s`, then distill.\n", lensName, lensName)
		default:
			fmt.Printf("No facets for lens %q yet — the reviewer runs after enough observations accumulate; force one now with `witness review`.\n", lensName)
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
