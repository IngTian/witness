package vector

import (
	"math"
	"sort"

	"github.com/IngTian/witness/internal/embed"
	"github.com/IngTian/witness/internal/store"
)

// This file adds the graph primitives the S3 emergent-arc hypothesis engine needs
// (issue #16 long-arc retrieval): a mutual-kNN adjacency over a lens's observation
// embeddings, and connected components over any adjacency. Both are pure and
// cosine-value-free where possible (ConnectedComponents needs no embeddings at all),
// so the clustering logic is unit-testable with synthetic vectors and hand-built
// graphs. No LLM, no store writes — this is the deterministic hypothesis half; the
// LLM verify step lives in internal/distill.
//
// Why mutual-kNN and not a cosine-threshold graph: an absolute-cosine cut over e5
// embeddings percolates to one giant component (e5's cone anisotropy puts arbitrary
// pairs at cosine ~0.7-0.85, so any value threshold near the median flips a huge
// fraction of edges). kNN is RANK-based, so the absolute value never enters the edge
// decision; requiring MUTUALITY (i in kNN(j) AND j in kNN(i)) drops one-way hub edges
// that would otherwise chain unrelated arcs into a blob. See the S3 design spec.

// MutualKNNAdj builds a mutual-k-nearest-neighbor graph over the observations of one
// lens. It returns the node slice (lens-filtered, empty-embedding rows dropped — same
// filter-first discipline as Search) and an adjacency list indexed into that slice: an
// undirected edge (i,j) exists iff j is among i's k nearest AND i is among j's k
// nearest (by cosine, within the lens). k is clamped to [1, len(nodes)-1].
func MutualKNNAdj(obs []store.Observation, lens string, k int) (nodes []store.Observation, adj [][]int) {
	// Filter FIRST (like Search): lens match + a usable embedding.
	for _, o := range obs {
		if lens != "" && o.Lens != lens {
			continue
		}
		if len(o.Embedding) == 0 {
			continue
		}
		nodes = append(nodes, o)
	}
	n := len(nodes)
	adj = make([][]int, n)
	if n < 2 {
		return nodes, adj
	}
	if k < 1 {
		k = 1
	}
	if k > n-1 {
		k = n - 1
	}

	// For each node, the set of its k nearest OTHER nodes (by cosine). O(n^2) — the
	// brute-force design point this package already documents (~5k vec/yr); the S3
	// pass is one-off/periodic, not per-session.
	knn := make([]map[int]bool, n)
	for i := range nodes {
		// Rank all other nodes by cosine to i, keep the top k indices.
		type sc struct {
			idx   int
			score float64
		}
		scores := make([]sc, 0, n-1)
		for j := range nodes {
			if j == i {
				continue
			}
			scores = append(scores, sc{j, embed.Cosine(nodes[i].Embedding, nodes[j].Embedding)})
		}
		// Rank by score descending, then take the top k. n is bounded (brute-force design
		// point); a full sort per node is fine. Tie-break on index so the graph is
		// deterministic regardless of input order.
		sort.Slice(scores, func(a, b int) bool {
			if scores[a].score != scores[b].score {
				return scores[a].score > scores[b].score
			}
			return scores[a].idx < scores[b].idx
		})
		set := make(map[int]bool, k)
		for t := 0; t < k && t < len(scores); t++ {
			set[scores[t].idx] = true
		}
		knn[i] = set
	}

	// Undirected edge iff mutual. Dedup with i<j.
	for i := 0; i < n; i++ {
		for j := range knn[i] {
			if j > i && knn[j][i] {
				adj[i] = append(adj[i], j)
				adj[j] = append(adj[j], i)
			}
		}
	}
	return nodes, adj
}

// ConnectedComponents returns the connected components of an undirected graph given
// as an adjacency list, each component as a slice of node indices. Union-find; no
// embeddings, no cosine — pure graph logic.
func ConnectedComponents(adj [][]int) [][]int {
	n := len(adj)
	parent := make([]int, n)
	for i := range parent {
		parent[i] = i
	}
	find := func(x int) int {
		for parent[x] != x {
			parent[x] = parent[parent[x]] // path halving
			x = parent[x]
		}
		return x
	}
	union := func(a, b int) {
		ra, rb := find(a), find(b)
		if ra != rb {
			parent[ra] = rb
		}
	}
	for i := 0; i < n; i++ {
		for _, j := range adj[i] {
			union(i, j)
		}
	}
	byRoot := make(map[int][]int)
	for i := 0; i < n; i++ {
		r := find(i)
		byRoot[r] = append(byRoot[r], i)
	}
	comps := make([][]int, 0, len(byRoot))
	for _, c := range byRoot {
		comps = append(comps, c)
	}
	return comps
}

// Centroid is the L2-normalized mean of the given embeddings — the query point for
// re-expanding a component (vector.Search around it). Empty/mismatched inputs return
// nil. Assumes members share a dimension (they do: one embedder).
func Centroid(members [][]float32) []float32 {
	if len(members) == 0 || len(members[0]) == 0 {
		return nil
	}
	d := len(members[0])
	sum := make([]float64, d)
	cnt := 0
	for _, v := range members {
		if len(v) != d {
			continue
		}
		for i := 0; i < d; i++ {
			sum[i] += float64(v[i])
		}
		cnt++
	}
	if cnt == 0 {
		return nil
	}
	var norm float64
	for i := 0; i < d; i++ {
		sum[i] /= float64(cnt)
		norm += sum[i] * sum[i]
	}
	out := make([]float32, d)
	if norm == 0 {
		return out
	}
	inv := 1.0 / math.Sqrt(norm)
	for i := 0; i < d; i++ {
		out[i] = float32(sum[i] * inv)
	}
	return out
}
