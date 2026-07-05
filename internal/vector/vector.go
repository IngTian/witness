// Package vector is a brute-force cosine index over L1 observation embeddings.
// At one-person scale (~5k vectors/yr) an ANN engine is overkill; this is ~50
// lines and exact. The same vectors serve read-time recall (MCP) and write-time
// dedup (the worker).
package vector

import (
	"sort"

	"github.com/IngTian/witness/internal/embed"
	"github.com/IngTian/witness/internal/store"
)

// Hit is one ranked observation.
type Hit struct {
	Obs   store.Observation
	Score float64
}

// Search ranks observations against a query embedding, optionally filtered to a
// single lens.
//
// CRITICAL: filter-then-rank, not rank-then-filter. If a rare lens (say math =
// 5% of the corpus) were filtered after a top-k cut, the top-k could be entirely
// another lens and the rare one would starve. We filter by lens first, then rank
// within that subset. (Free at brute-force scale; invisible until a tag goes rare.)
func Search(obs []store.Observation, query []float32, lens string, k int) []Hit {
	hits := make([]Hit, 0, len(obs))
	for _, o := range obs {
		if lens != "" && o.Lens != lens { // filter FIRST
			continue
		}
		if len(o.Embedding) == 0 {
			continue
		}
		hits = append(hits, Hit{Obs: o, Score: embed.Cosine(query, o.Embedding)})
	}
	sort.Slice(hits, func(i, j int) bool { return hits[i].Score > hits[j].Score })
	if k > 0 && len(hits) > k {
		hits = hits[:k]
	}
	return hits
}

// NearestScore returns the best cosine score of candidate against any existing
// observation in the same lens — the dedup signal the worker uses before
// appending a mined observation (near-duplicate => skip / fold).
func NearestScore(existing []store.Observation, candidate []float32, lens string) float64 {
	best := 0.0
	for _, o := range existing {
		if o.Lens != lens || len(o.Embedding) == 0 {
			continue
		}
		if s := embed.Cosine(candidate, o.Embedding); s > best {
			best = s
		}
	}
	return best
}
