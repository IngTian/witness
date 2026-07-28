package vector

import (
	"math"
	"sort"
	"testing"

	"github.com/IngTian/witness/internal/store"
)

// ConnectedComponents is a cosine-free union-find over an adjacency list — the graph
// half of the S3 emergent-arc clustering (issue #16 / long-arc retrieval). These
// tests pin it with hand-built adjacencies so the graph logic is provable without
// any embeddings.
func TestConnectedComponents(t *testing.T) {
	// Nodes 0..5: {0,1,2} a triangle, {3,4} an edge, {5} isolated.
	adj := [][]int{
		{1, 2}, // 0
		{0, 2}, // 1
		{0, 1}, // 2
		{4},    // 3
		{3},    // 4
		{},     // 5
	}
	comps := normalizeComps(ConnectedComponents(adj))
	want := [][]int{{0, 1, 2}, {3, 4}, {5}}
	if !equalComps(comps, want) {
		t.Fatalf("components = %v, want %v", comps, want)
	}
}

func TestConnectedComponentsChain(t *testing.T) {
	// A chain 0-1-2-3 is ONE component even though no node touches all others.
	adj := [][]int{{1}, {0, 2}, {1, 3}, {2}}
	comps := normalizeComps(ConnectedComponents(adj))
	if len(comps) != 1 || len(comps[0]) != 4 {
		t.Fatalf("a chain must be one component of 4, got %v", comps)
	}
}

// unitVec L2-normalizes a direction so cosine similarity is controllable by construction.
func unitVec(dir []float32) []float32 {
	var norm float64
	for _, x := range dir {
		norm += float64(x) * float64(x)
	}
	if norm == 0 {
		return dir
	}
	inv := float32(1.0 / math.Sqrt(norm))
	out := make([]float32, len(dir))
	for i, x := range dir {
		out[i] = x * inv
	}
	return out
}

func obsAt(id, lens string, session string, dir []float32) store.Observation {
	return store.Observation{ID: id, Lens: lens, Session: session, Embedding: unitVec(dir)}
}

// TestMutualKNNDropsHubEdges: two tight groups plus one HUB obs sitting between them.
// Plain kNN would chain the two groups through the hub into one component; mutual-kNN
// requires reciprocity, so the hub's one-way edges are dropped and the groups stay
// SEPARATE. Locks the mutuality property (the anti-blob mechanism).
func TestMutualKNNDropsHubEdges(t *testing.T) {
	// Group A near direction (1,0,0), group B near (0,1,0); hub near (1,1,0) (between).
	obs := []store.Observation{
		obsAt("a1", "l", "s1", []float32{1, 0, 0}),
		obsAt("a2", "l", "s2", []float32{0.98, 0.02, 0}),
		obsAt("a3", "l", "s3", []float32{0.97, 0.03, 0}),
		obsAt("b1", "l", "s4", []float32{0, 1, 0}),
		obsAt("b2", "l", "s5", []float32{0.02, 0.98, 0}),
		obsAt("b3", "l", "s6", []float32{0.03, 0.97, 0}),
		obsAt("hub", "l", "s7", []float32{1, 1, 0}), // equidistant-ish to both groups
	}
	nodes, adj := MutualKNNAdj(obs, "l", 2)
	comps := ConnectedComponents(adj)
	// Map components back to id-sets.
	var sets [][]string
	for _, c := range comps {
		var ids []string
		for _, ni := range c {
			ids = append(ids, nodes[ni].ID)
		}
		sort.Strings(ids)
		sets = append(sets, ids)
	}
	// The two tight groups must NOT be merged into one 6+ member component.
	for _, s := range sets {
		if len(s) >= 6 {
			t.Fatalf("mutual-kNN failed to keep groups separate; got a merged component %v", s)
		}
	}
	// And a-group members should land together, b-group together.
	comp := map[string]int{}
	for ci, s := range sets {
		for _, id := range s {
			comp[id] = ci
		}
	}
	if comp["a1"] != comp["a2"] || comp["a2"] != comp["a3"] {
		t.Fatalf("group A split across components: %v", sets)
	}
	if comp["b1"] != comp["b2"] || comp["b2"] != comp["b3"] {
		t.Fatalf("group B split across components: %v", sets)
	}
	if comp["a1"] == comp["b1"] {
		t.Fatalf("groups A and B were merged (hub chaining not prevented): %v", sets)
	}
}

// MutualKNNAdj must filter to the lens first (like Search) and skip empty embeddings.
func TestMutualKNNFiltersLensAndSkipsEmpty(t *testing.T) {
	obs := []store.Observation{
		obsAt("a1", "math", "s1", []float32{1, 0, 0}),
		obsAt("a2", "math", "s2", []float32{0.99, 0.01, 0}),
		{ID: "noemb", Lens: "math", Session: "s3"},          // empty embedding — skipped
		obsAt("other", "default", "s4", []float32{1, 0, 0}), // wrong lens — excluded
	}
	nodes, adj := MutualKNNAdj(obs, "math", 3)
	if len(nodes) != 2 {
		t.Fatalf("nodes = %d, want 2 (lens-filtered, empty-embedding dropped)", len(nodes))
	}
	if len(adj) != len(nodes) {
		t.Fatalf("adj length %d != nodes %d", len(adj), len(nodes))
	}
	for _, n := range nodes {
		if n.Lens != "math" || n.ID == "noemb" {
			t.Fatalf("unexpected node %q lens=%q", n.ID, n.Lens)
		}
	}
}

// --- test helpers ---

func normalizeComps(comps [][]int) [][]int {
	out := make([][]int, 0, len(comps))
	for _, c := range comps {
		cc := append([]int(nil), c...)
		sort.Ints(cc)
		out = append(out, cc)
	}
	sort.Slice(out, func(i, j int) bool { return out[i][0] < out[j][0] })
	return out
}

func equalComps(a, b [][]int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if len(a[i]) != len(b[i]) {
			return false
		}
		for j := range a[i] {
			if a[i][j] != b[i][j] {
				return false
			}
		}
	}
	return true
}
