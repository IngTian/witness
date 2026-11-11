package commands

import (
	"fmt"
	"slices"
	"strings"

	"github.com/IngTian/witness/internal/embed"
	"github.com/IngTian/witness/internal/lens"
	"github.com/IngTian/witness/internal/store"
	"github.com/IngTian/witness/internal/vector"
	"github.com/spf13/cobra"
)

func newObservationsCmd() *cobra.Command {
	obsCmd := &cobra.Command{
		Use:     "observations",
		GroupID: groupRead,
		Short:   "Search what witness has noticed about you.",
		Long:    "Search the specific things witness has noticed about your work patterns, preferences, and growth. Each observation is timestamped and organized by lens and dimension.",
		RunE:    groupRunE, // a typo'd subcommand is an error, not its help page and exit 0
	}

	var searchLens string
	var searchK int
	var searchJSON bool
	search := &cobra.Command{
		Use:   "search <query>",
		Short: "Search what witness has noticed, by meaning.",
		Long:  "Search the things witness has observed about you, finding semantically similar observations via local embedding search.",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			return cmdObservationSearch(args[0], searchLens, searchK, searchJSON)
		},
	}
	search.Flags().StringVar(&searchLens, "lens", store.LensDefault, "lens to search")
	search.Flags().IntVarP(&searchK, "limit", "k", 8, "max results")
	search.Flags().BoolVarP(&searchJSON, "json", "j", false, "output as JSON")

	// No `record` sibling: MCP record_observation is the one write path, with the input checks
	// (size caps, lens-name screening) a hand-copied CLI writer had skipped (#199 F049).
	deleteCmd := &cobra.Command{
		Use:    "delete <obs_id>",
		Short:  "Delete one observation.",
		Long:   "Delete one L1 observation by obs_id. It is removed now; facets that rested only on it are retired at the next review, and nothing is re-read. This is the CLI equivalent of the MCP delete_observation tool.",
		Args:   cobra.ExactArgs(1),
		Hidden: true,
		RunE: func(_ *cobra.Command, args []string) error {
			return cmdObservationDelete(args[0])
		},
	}

	obsCmd.AddCommand(search, deleteCmd)
	return obsCmd
}

func cmdObservationSearch(query, lensName string, k int, asJSON bool) error {
	query = strings.TrimSpace(query)
	if query == "" {
		return fmt.Errorf("query is required")
	}
	lensName = strings.TrimSpace(lensName)
	if lensName == "" {
		lensName = store.LensDefault
	}
	if k <= 0 {
		k = 8
	}
	st, err := store.Open()
	if err != nil {
		return err
	}
	defer st.Close()
	// Observations carry the lens's mined name, which a registered lens may override. Read only
	// that lens: Search filters by it anyway, and every other lens's embeddings cost ~5x the memory.
	dataName := lens.DataName(st, lensName)
	obs, err := st.ReadObservations(dataName)
	if err != nil {
		return err
	}
	emb, err := embed.New()
	if err != nil {
		return err
	}
	qv, err := emb.Embed(query)
	if err != nil {
		return err
	}
	hits := vector.Search(obs, qv, dataName, k)
	if asJSON {
		out := observationSearchJSON{Lens: lensName, Query: query, Limit: k, Hits: []observationHitJSON{}}
		for _, h := range hits {
			o := h.Obs
			out.Hits = append(out.Hits, observationHitJSON{
				ID:          o.ID,
				TS:          o.TS,
				Session:     o.Session,
				Lens:        o.Lens,
				Dimension:   o.Dimension,
				Observation: o.Observation,
				Evidence:    o.Evidence,
				Poignancy:   o.Poignancy,
				Score:       h.Score,
			})
		}
		return emitJSON(out)
	}
	renderObservationHits(lensName, query, hits,
		slices.Contains(st.RegisteredLenses(), lensName), slices.Contains(st.LoadConfig().EnabledLenses, lensName),
		func() store.EmptyReason { return st.WhyEmpty(dataName) })
	return nil
}

type observationSearchJSON struct {
	Lens  string               `json:"lens"`
	Query string               `json:"query"`
	Limit int                  `json:"limit"`
	Hits  []observationHitJSON `json:"hits"`
}

type observationHitJSON struct {
	ID          string  `json:"id"`
	TS          string  `json:"ts"`
	Session     string  `json:"session,omitempty"`
	Lens        string  `json:"lens"`
	Dimension   string  `json:"dimension"`
	Observation string  `json:"observation"`
	Evidence    string  `json:"evidence,omitempty"`
	Poignancy   int     `json:"poignancy"`
	Score       float64 `json:"score"`
}

// renderObservationHits prints the hits most relevant first. lensName is what the user typed;
// registered/enabled decide which next step an empty result can honestly suggest.
func renderObservationHits(lensName, query string, hits []vector.Hit, registered, enabled bool, whyEmpty func() store.EmptyReason) {
	if len(hits) == 0 {
		fmt.Println(title("observations", lensName, fmt.Sprintf("no hits for %q", query)))
		fmt.Println()
		// vector.Search is top-k with no score threshold, so zero hits never means "your
		// query missed" — it means there is nothing embedded to search. Say which layer is
		// actually missing instead of leaving the user to guess (#166). An unregistered lens
		// comes first: every other step (backfill, doctor) would fail or mislead for it.
		reason := store.NoCapture
		if registered {
			reason = whyEmpty()
		}
		switch {
		case !registered:
			fmt.Printf("  %s no lens %q is registered\n", skipGlyph(), lensName)
			fmt.Println(hint("see lenses", "witness lens list"))
		case reason == store.NoCapture:
			fmt.Printf("  %s nothing to search — nothing is capturing (the archive holds no raw turns)\n", skipGlyph())
			fmt.Println(hint("wire editor", wireCommand()))
		case reason == store.NoObservations:
			fmt.Printf("  %s nothing has been mined into %q yet\n", skipGlyph(), lensName)
			if !enabled {
				fmt.Println(hint("enable it", "witness lens enable "+lensName))
			}
			fmt.Println(hint("mine history", "witness lens backfill "+lensName))
		default:
			fmt.Printf("  %s %q has observations, but none carry embeddings, so nothing is searchable\n", warnGlyph(), lensName)
			fmt.Println(hint("diagnose", "witness doctor"))
		}
		return
	}
	fmt.Println(title("observations", lensName, fmt.Sprintf("%s for %q", countOf(len(hits), "hit", "hits"), query)))
	for _, h := range hits {
		o := h.Obs
		// 2 indent + 5 score + 2 gap = the 9-column hanging indent every line of a hit shares.
		fmt.Println()
		fmt.Printf("  %s  %s\n", bold(cyan(fmt.Sprintf("%.3f", h.Score))), dim(o.Dimension+" · "+shortTime(valueOrNever(o.TS))))
		fmt.Printf("         %s\n", fitLine(o.Observation, 9))
		if o.Evidence != "" {
			fmt.Printf("         %s\n", dim(glyph("↳", "->")+" "+fitLine(o.Evidence, 11)))
		}
		ids := o.ID
		if o.Session != "" {
			ids += " · session " + o.Session
		}
		fmt.Printf("         %s\n", dim(ids))
	}
}

func cmdObservationDelete(obsID string) error {
	obsID = strings.TrimSpace(obsID)
	if obsID == "" {
		return fmt.Errorf("obs_id is required")
	}
	st, err := store.Open()
	if err != nil {
		return err
	}
	defer st.Close()
	deleted, err := st.DeleteObservation(obsID)
	if err != nil {
		return err
	}
	if !deleted {
		// Exit 0 on purpose: deleting what is already gone is idempotent, the MCP tool's contract too.
		fmt.Println(result(skipGlyph(), "no observation "+obsID, "nothing deleted"))
		return nil
	}
	fmt.Println(result(okGlyph(), "deleted "+obsID, "facets that rested only on it are retired at the next review"))
	return nil
}
