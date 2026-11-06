package commands

import (
	"path/filepath"
	"testing"

	"github.com/IngTian/witness/internal/store"
)

// ensureProfileFresh's order is its contract: lock, then lenses, then prompts, then a runner.
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
	if ls, _ := activeLenses(st); len(ls) != 0 {
		t.Fatalf("precondition: no lens enabled, got %d", len(ls))
	}

	if ran, err := ensureProfileFresh(st); ran || err != nil {
		t.Errorf("ensureProfileFresh = (%v, %v), want (false, nil)", ran, err)
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
	if ls, _ := activeLenses(st); len(ls) == 0 {
		t.Fatal("precondition: the default lens should be enabled")
	}
	t.Setenv("WITNESS_PROMPTS", t.TempDir())
	t.Setenv("PATH", t.TempDir())

	unlock, ok := st.WorkerLock()
	if !ok {
		t.Fatal("could not take the worker lock")
	}
	ran, err := ensureProfileFresh(st)
	unlock()
	if ran || err != nil {
		t.Errorf("with the lock held: ensureProfileFresh = (%v, %v), want (false, nil)", ran, err)
	}

	if ran, err := ensureProfileFresh(st); ran || err == nil {
		t.Errorf("control: without the lock the broken prompts should fail, got (%v, %v)", ran, err)
	}
}
