package commands

import (
	"context"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/IngTian/witness/internal/platform"
	"github.com/IngTian/witness/internal/store"
)

// autoClaimFakePlatform answers a claim batch (its input carries "LENS: <name>\nFACETS: N", inside
// RunnerMine's fence) with a claim for facet traits/k, and every other call with "[]".
// In-process: no real CLI is spawned.
type autoClaimFakePlatform struct{ leakFakePlatform }

func (autoClaimFakePlatform) Name() string                           { return "autoclaim" }
func (autoClaimFakePlatform) SessionPrefix() string                  { return "autoclaim:" }
func (autoClaimFakePlatform) NewRunner(store.Config) platform.Runner { return autoClaimFakeRunner{} }

type autoClaimFakeRunner struct{ leakFakeRunner }

func (autoClaimFakeRunner) Run(_ context.Context, _, _, input string) (string, error) {
	if strings.Contains(input, "LENS: test\nFACETS: ") {
		return `[{"dimension":"traits","key":"k","claim":"CLAIM-k"}]`, nil
	}
	return "[]", nil
}

var registerAutoClaimOnce sync.Once

// `witness worker review` must run the automatic claim pass before folding (#199 PR A), so a lens
// that was never compacted gets claims and its review shrinks. This drives the real review path
// end to end and asserts the claim landed in the store.
func TestForegroundReviewWritesMissingClaims(t *testing.T) {
	registerAutoClaimOnce.Do(func() { platform.Register(autoClaimFakePlatform{}) })
	t.Setenv("WITNESS_HOME", filepath.Join(t.TempDir(), "witness"))
	st, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.SetConfigString("runner", "autoclaim"); err != nil {
		t.Fatal(err)
	}
	if err := st.RegisterLens("test", writeLensDir(t, "test", "Extract prompt.", "Review prompt.")); err != nil {
		t.Fatal(err)
	}
	if err := st.EnableLens("test"); err != nil {
		t.Fatal(err)
	}
	if err := st.WriteFacets([]store.Facet{{Lens: "test", Dimension: "traits", Key: "k",
		Versions: []store.FacetVersion{{Value: "a long value with no claim yet", Confidence: 0.9}}}}); err != nil {
		t.Fatal(err)
	}
	if err := st.AppendObservations([]store.Observation{{ID: store.ObservationID("s", "test", "o"),
		TS: time.Now().UTC().Format(time.RFC3339), Session: "s", Lens: "test", Dimension: "traits",
		Observation: "did a thing", Poignancy: 5}}); err != nil {
		t.Fatal(err)
	}

	if ran, _, err := forceReviewTapped(st, false, &causeTap{}); err != nil || !ran {
		t.Fatalf("review: ran=%v err=%v", ran, err)
	}
	fs, err := st.ReadFacets()
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range fs {
		if f.Lens == "test" && f.Key == "k" {
			if got := f.Current().Claim; got != "CLAIM-k" {
				t.Fatalf("claim = %q, want CLAIM-k: the review did not run the claim pass", got)
			}
			return
		}
	}
	t.Fatal("facet test/traits/k vanished")
}

// Both review paths — `witness worker review` (review.go) and the drain (worker.go) — must build
// their Reviewer with autoClaimer. The drain's review fires only when a review is due, which needs
// a long fixture; this pins that neither construction site can drop the claim pass silently.
func TestBothReviewPathsWireTheClaimPass(t *testing.T) {
	for _, file := range []string{"review.go", "worker.go"} {
		src := readSource(t, file)
		if !strings.Contains(src, "Claims: autoClaimer(") && !strings.Contains(src, "Claims: claims}") {
			t.Errorf("%s builds a distill.Reviewer without the automatic claim pass", file)
		}
	}
}
