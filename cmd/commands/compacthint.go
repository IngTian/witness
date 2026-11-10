package commands

import (
	"strconv"

	"github.com/IngTian/witness/internal/lens"
	"github.com/IngTian/witness/internal/store"
)

// Compaction stays MANUAL: it merges and retires facets, which changes what the profile says, so
// it shows a plan and asks first. doctor and status only point at it once a lens has grown (#199):
//   - never compacted, with at least compactHintMin active facets; or
//   - grown to compactHintGrowth × the count the last compaction left (CompactBaselineKey).
//
// A lens compacted before the baseline existed (it has archived facets but no key) gets no hint
// until its next compaction records one: there is no honest "grown since" to report, and calling
// it "never compacted" would be false.
//
// Claims, which shrink the review call, are written automatically before every fold. This hint
// is about the facet COUNT, which only a compaction reduces.
const (
	compactHintMin    = 300
	compactHintGrowth = 1.5
)

type compactNudge struct {
	Lens     string // the registered name, which `witness compact --lens` takes
	Active   int
	Baseline int // 0 = never compacted
}

func (n compactNudge) text() string {
	facets := countOf(n.Active, "active facet", "active facets")
	if n.Baseline == 0 {
		return n.Lens + ": " + facets + ", never compacted"
	}
	return n.Lens + ": " + facets + ", up from " + humanCount(n.Baseline) + " after the last compaction"
}

func (n compactNudge) command() string { return "witness compact --lens " + n.Lens }

// compactionNudges reads the enabled lenses' facet counts by their data names (a lens can mine
// under a different name than it is registered as) and reports the ones worth compacting.
func compactionNudges(st *store.Store) []compactNudge {
	var out []compactNudge
	for _, name := range st.LoadConfig().EnabledLenses {
		active, archived := st.LensFacetCounts(lens.DataName(st, name))
		if active < compactHintMin {
			continue
		}
		base, _ := strconv.Atoi(st.MetaString(store.CompactBaselineKey(lens.DataName(st, name))))
		switch {
		case base > 0:
			if float64(active) >= compactHintGrowth*float64(base) {
				out = append(out, compactNudge{Lens: name, Active: active, Baseline: base})
			}
		case archived == 0:
			out = append(out, compactNudge{Lens: name, Active: active})
		}
	}
	return out
}

// stampCompactBaselines records each lens's active facet count after a compaction ran on it,
// including one that proposed nothing (otherwise the hint would nag about a lens compaction has
// already judged). Best-effort: a failed stamp only means the hint may fire early.
func stampCompactBaselines(st *store.Store, lenses []*lens.Lens) {
	for _, ln := range lenses {
		active, _ := st.LensFacetCounts(ln.Name)
		_ = st.SetMetaString(store.CompactBaselineKey(ln.Name), strconv.Itoa(active))
	}
}
