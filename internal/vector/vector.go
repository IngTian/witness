// Package vector is a brute-force cosine index over L1 observation embeddings.
// At one-person scale (~5k vectors/yr) an ANN engine is overkill; this is ~40
// lines and exact.
//
// It serves READ-TIME RECALL only (MCP search_observations, `witness observations search`). It used
// to also serve write-time dedup via NearestScore, which scanned the whole corpus per mined
// observation — the O(n^2) ceiling that issue #85 is built around. Append-only L1 replaced that with
// an exact-ID hash set in CommitMining (worker.go), so NearestScore lost its only caller and has
// been removed; #85's headline ceiling no longer exists in the code it describes.
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
