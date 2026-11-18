package commands

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/IngTian/witness/internal/store"
)

// ensureProfileFresh's order is its contract: lock, then lenses, then prompts, then the archive
// check, then a runner (on the first due call only).
// Both tests break the prompts (an empty WITNESS_PROMPTS) so any step taken out of order shows
// up as an error, and empty PATH so nothing could launch a runner even if one were reached.

// No enabled lens returns before anything else is read: a fresh archive must not fail a read
// over prompts it would never use.
func TestProfilePullZeroLensesNeverLoadsPrompts(t *testing.T) {
	t.Setenv("WITNESS_HOME", t.TempDir())
	t.Setenv("WITNESS_NO_DEFAULT_LENS", "1")
	t.Setenv("WITNESS_PROMPTS", t.TempDir())
	t.Setenv("PATH", t.TempDir())
	st, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if ls := activeLenses(st); len(ls) != 0 {
		t.Fatalf("precondition: no lens enabled, got %d", len(ls))
	}

	if err := ensureProfileFresh(st); err != nil {
		t.Errorf("ensureProfileFresh = %v, want nil", err)
	}
}

// A held WorkerLock serves the cached profile before anything else is read. The control call
// after unlocking shows the same archive DOES fail on its prompts, so the lock is what spared it.
func TestProfilePullLockHeldTouchesNothing(t *testing.T) {
	t.Setenv("WITNESS_HOME", t.TempDir())
	t.Setenv("WITNESS_PROMPTS", filepath.Join("..", "..", "prompts")) // seeds the default lens
	st, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if ls := activeLenses(st); len(ls) == 0 {
		t.Fatal("precondition: the default lens should be enabled")
	}
	t.Setenv("WITNESS_PROMPTS", t.TempDir())
	t.Setenv("PATH", t.TempDir())

	unlock, ok := st.WorkerLock()
	if !ok {
		t.Fatal("could not take the worker lock")
	}
	err = ensureProfileFresh(st)
	unlock()
	if err != nil {
		t.Errorf("with the lock held: ensureProfileFresh = %v, want nil", err)
	}

	if err := ensureProfileFresh(st); err == nil {
		t.Error("control: without the lock the broken prompts should fail, got nil")
	}
}

// `profile --json` still serves the cached profile when the refresh fails, but says so: it used
// to drop the error, so a script read a stale profile as fresh.
func TestProfileJSONReportsARefreshFailure(t *testing.T) {
	t.Setenv("WITNESS_HOME", t.TempDir())
	t.Setenv("WITNESS_PROMPTS", filepath.Join("..", "..", "prompts")) // seeds the default lens
	st, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	st.Close()
	t.Setenv("WITNESS_PROMPTS", t.TempDir()) // broken prompts: the refresh fails before any runner
	t.Setenv("PATH", t.TempDir())

	out, err := captureStdoutErr(t, func() error { return cmdProfile(nil, true) })
	if err != nil {
		t.Fatalf("a failed refresh must not fail the read: %v", err)
	}
	var got profileJSON
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, out)
	}
	if got.RefreshError == "" {
		t.Errorf("refresh_error is empty, so the failure is invisible to a script:\n%s", out)
	}
}

// changeMathFacet replaces compactFixture's facets with one math facet of new text, so the math
// portrait (and the unified one over it) is due again.
func changeMathFacet(t *testing.T, st *store.Store) {
	t.Helper()
	if err := st.WriteFacets([]store.Facet{{Lens: "math", Dimension: "thinking", Key: "a", LastSeen: "2026-10-02T00:00:00Z",
		Versions: []store.FacetVersion{{Value: "a changed value", Confidence: 0.7, RecordedAt: "2026-10-02T00:00:00Z",
			ValidFrom: "2026-10-02T00:00:00Z", BecauseOf: []string{"obs-a"}}}}}); err != nil {
		t.Fatal(err)
	}
}

// get_profile's note is Summarize's own skip test: it says stale exactly when `witness profile`
// would rewrite or remove the portrait, and it takes no lock, so a running drain does not hide it.
func TestMCPProfileStaleIsSummarizesSkipTest(t *testing.T) {
	compactFixture(t)
	t.Setenv("WITNESS_PROMPTS", filepath.Join("..", "..", "prompts"))
	st, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := ensureProfileFresh(st); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"math", store.ProfileUnified} {
		if _, ok, _ := st.ReadProfile(n); !ok {
			t.Fatalf("precondition: %s.md was not written", n)
		}
	}
	stale := mcpOptions(st).ProfileStale
	agree := func(when string) {
		t.Helper()
		sm, err := profileSummarizer(st)
		if err != nil {
			t.Fatal(err)
		}
		want, err := sm.Stale()
		if err != nil {
			t.Fatal(err)
		}
		for _, n := range []string{"math", store.ProfileUnified} {
			if got := stale(n); got != want[n] {
				t.Errorf("%s: ProfileStale(%s) = %v, Summarizer.Stale says %v", when, n, got, want[n])
			}
		}
	}

	if stale("math") || stale(store.ProfileUnified) {
		t.Error("portraits just written read as stale")
	}
	agree("fresh")
	changeMathFacet(t, st)
	if !stale("math") || !stale(store.ProfileUnified) {
		t.Error("a changed facet must make the math and unified portraits stale")
	}
	agree("after a facet changed")
	if err := st.WriteProfile("gone", "# old"); err != nil {
		t.Fatal(err)
	}
	if !stale("gone") {
		t.Error("a portrait whose lens has no facets is one Summarize would reap, so it is stale")
	}
	if stale("never") {
		t.Error("a portrait that is neither on disk nor due is not stale")
	}
	unlock, ok := st.WorkerLock()
	if !ok {
		t.Fatal("could not take the worker lock")
	}
	if !stale("math") {
		t.Error("a held worker lock must not hide a stale portrait")
	}
	unlock()
}

// `witness profile` opens the runner set only when a portrait is due: a read of fresh portraits
// costs no runner start, and a runner that cannot open is reported as itself, not as a portrait
// input that outgrew the model.
func TestProfileFreshReadOpensNoRunner(t *testing.T) {
	compactFixture(t)
	t.Setenv("WITNESS_PROMPTS", filepath.Join("..", "..", "prompts"))
	st, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	opens := atomic.LoadInt32(&leakFakeOpens)
	if err := ensureProfileFresh(st); err != nil || atomic.LoadInt32(&leakFakeOpens)-opens != 1 {
		t.Fatalf("no portrait yet: err %v, %d opens (want nil, 1)", err, atomic.LoadInt32(&leakFakeOpens)-opens)
	}
	opens = atomic.LoadInt32(&leakFakeOpens)
	if err := ensureProfileFresh(st); err != nil || atomic.LoadInt32(&leakFakeOpens)-opens != 0 {
		t.Fatalf("fresh portraits: err %v, %d opens (want nil, 0)", err, atomic.LoadInt32(&leakFakeOpens)-opens)
	}

	if err := st.SetConfigString("runner", "nosuch"); err != nil {
		t.Fatal(err)
	}
	changeMathFacet(t, st)
	if err := ensureProfileFresh(st); err == nil || strings.Contains(err.Error(), "context window") {
		t.Errorf("a runner that cannot open must be reported bare, got %v", err)
	}
}
