package commands

import (
	"fmt"
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
	renderCurrentFacets(lensName, facets)
	return nil
}

type facetsJSON struct {
	Lens   string        `json:"lens"`
	Facets []store.Facet `json:"facets"`
}

func renderCurrentFacets(lensName string, facets []store.Facet) {
	count := 0
	for _, f := range facets {
		if f.Lens == lensName && f.Current() != nil {
			count++
		}
	}
	if count == 0 {
		fmt.Printf("No facets found for lens %q. The reviewer may not have run yet.\n", lensName)
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
