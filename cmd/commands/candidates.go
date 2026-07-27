package commands

import (
	"fmt"
	"strings"

	"github.com/IngTian/witness/internal/distill"
	"github.com/IngTian/witness/internal/store"
)

// cmdCandidates is the S3 dry-run inspector (issue #16 long-arc retrieval): it runs the
// PURE emergent-arc clustering over a lens's L1 and PRINTS the proposed candidate arcs —
// no LLM call, no writes, nothing persisted. It exists so cluster quality can be judged
// on a real archive before any verify budget is spent: are the proposed clusters
// coherent, multi-session, and not-already-named? If the geometry is bad, we see it here
// for free. This is the whole S3a slice's payoff.
func cmdCandidates(lens string) error {
	st, err := store.Open()
	if err != nil {
		return err
	}
	defer st.Close()

	obs, err := st.ReadObservations(lens) // embeddings decoded — clustering needs the vectors
	if err != nil {
		return err
	}
	facets, err := st.ReadFacets()
	if err != nil {
		return err
	}

	cands := distill.Candidates(obs, facets, lens)
	total := 0
	for _, o := range obs {
		if o.Lens == lens {
			total++
		}
	}
	fmt.Printf("lens %s: %d observations → %d candidate arc(s) (dry run — nothing written)\n\n",
		bold(lens), total, len(cands))
	if len(cands) == 0 {
		fmt.Println(dim("  no emergent arcs proposed (no dense cross-session cluster the current facets don't already cover)"))
		return nil
	}
	for i, c := range cands {
		coverage := fmt.Sprintf("%.0f%%", c.BestFacetCoverage*100)
		cov := coverage
		if c.CoveringFacet != "" {
			cov = coverage + " of " + c.CoveringFacet
		}
		fmt.Printf("%s  %d obs · %d sessions · %d days · %s..%s · coverage %s\n",
			bold(fmt.Sprintf("arc %d", i+1)), len(c.Members), len(c.Sessions), c.DistinctDays,
			shortTS(c.SpanFrom), shortTS(c.SpanTo), cov)
		// Show a few member observations so a human can eyeball coherence.
		for j, m := range c.Members {
			if j >= 5 {
				fmt.Printf("      %s\n", dim(fmt.Sprintf("… +%d more", len(c.Members)-5)))
				break
			}
			fmt.Printf("      %s %s\n", dim("·"), truncate(m.Observation, 88))
		}
		fmt.Println()
	}
	return nil
}

func shortTS(ts string) string {
	if len(ts) >= 10 {
		return ts[:10]
	}
	return ts
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}
