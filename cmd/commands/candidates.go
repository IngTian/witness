package commands

import (
	"fmt"
	"slices"
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
	total := 0
	for _, o := range obs {
		if o.Lens == lens {
			total++
		}
	}
	// A typo'd lens used to draw a clustering bar and report "0 observations → 0 candidate arc(s)"
	// as if the lens existed. A lens that is not registered but still has observations (it was
	// deregistered) is still worth inspecting, so refuse only when there is neither.
	if total == 0 && !slices.Contains(st.RegisteredLenses(), lens) {
		return fmt.Errorf("lens %q is not registered (see `witness lens list`)", lens)
	}
	facets, err := st.ReadActiveFacets()
	if err != nil {
		return err
	}

	// Progress on stderr: no model call here, but on a real 17,413-observation lens the clustering
	// alone took 38 minutes, and it used to sit silent for all of it.
	pl := startProgress()
	cands := distill.Candidates(obs, facets, lens, func(done, total int) {
		pl.update(distill.Progress{Where: lens, Stages: []distill.Stage{{Name: "clustering", Done: done, Total: total}}})
	})
	pl.finish()
	fmt.Printf("lens %s: %s → %s %s\n\n", bold(lens), countOf(total, "observation", "observations"),
		countOf(len(cands), "candidate arc", "candidate arcs"), dim("(dry run — nothing written)"))
	if len(cands) == 0 {
		fmt.Println(dim("  no emergent arcs proposed (no dense cross-session cluster the current facets don't already cover)"))
		return nil
	}
	for i, c := range cands {
		cov := fmt.Sprintf("%.0f%%", c.BestFacetCoverage*100)
		if c.CoveringFacet != "" {
			// "dimension|key" internally; every other surface writes a facet as dimension/key.
			cov += " of " + strings.Replace(c.CoveringFacet, "|", "/", 1)
		}
		fmt.Printf("%s  %s · %s · %s · %s → %s · coverage %s\n",
			bold(fmt.Sprintf("arc %d", i+1)), countOf(len(c.Members), "obs", "obs"),
			countOf(len(c.Sessions), "session", "sessions"), countOf(c.DistinctDays, "day", "days"),
			shortTS(c.SpanFrom), shortTS(c.SpanTo), cov)
		// Show a few member observations so a human can eyeball coherence.
		for j, m := range c.Members {
			if j >= 5 {
				fmt.Printf("      %s\n", dim("… +"+humanCount(len(c.Members)-5)+" more"))
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

// truncate cuts s to n characters with an ellipsis — by RUNES: cutting bytes split a multi-byte
// character (an em dash, any CJK text) and printed the broken half as U+FFFD. 161 observations in
// one real archive would have been cut that way.
func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}
