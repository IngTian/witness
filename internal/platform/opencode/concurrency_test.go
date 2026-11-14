package opencode

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"
)

// Generations on one serve run in parallel, up to maxConcurrentGenerations (8 verified on one
// serve, critic G4; witness allows 16 miners). The cap is read when the server is built, so a test
// (and PR 3's 16-way check) sets it first. maxInFlight == the cap proves both halves: never above
// it, and not collapsed to one.
func TestRunCapsConcurrentGenerations(t *testing.T) {
	prev := maxConcurrentGenerations
	maxConcurrentGenerations = 2
	t.Cleanup(func() { maxConcurrentGenerations = prev })
	fake, srv := newFakeV2(t)
	fake.turn = func(s *fakeSession) []map[string]any {
		if time.Since(s.Prompted) < 100*time.Millisecond {
			return []map[string]any{s.user(), assistantRec("", nil)}
		}
		return okTurn("RESULT")(s)
	}
	var wg sync.WaitGroup
	errs := make([]error, 6)
	for i := range errs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errs[i] = srv.Run(context.Background(), "openai/gpt-5.5", "P", fmt.Sprint(i))
		}()
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Errorf("Run %d: %v", i, err)
		}
	}
	if got := fake.peak(); got != 2 {
		t.Errorf("max generations in flight = %d, want exactly the cap (2)", got)
	}
}

// The closed check still holds with the narrow lock: a Run after Close errors before any request.
func TestOpenCodeServerRunRejectsAfterClose(t *testing.T) {
	fake, srv := newFakeV2(t)
	if err := srv.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := srv.Run(context.Background(), "openai/gpt-5.5", "EXTRACT", "DATA"); err == nil {
		t.Fatal("Run on a closed server should error")
	}
	if calls := fake.seen(); len(calls) != 0 {
		t.Errorf("a closed server sent %v", calls)
	}
}
