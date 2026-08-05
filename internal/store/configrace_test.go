package store

import (
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
)

// Every config.toml write is a read-modify-write of the whole file. Unserialized, two
// writers both read the pre-edit file and the second rename silently discards the first's
// edit while BOTH report success. The stakes are not cosmetic: losing the auto-seeded
// `lens = default` line permanently disables all distillation, because defaultseed.go has
// already burned its one-shot marker so nothing re-seeds it, PendingSessions returns nil
// on an empty lens set, and doctor then prints [ok] with 0 pending forever.
//
// Measured before the fix: 22 silent lost updates and 98 "rename config.toml.tmp: no such
// file or directory" errors across 20 trials (the shared .tmp path let writers truncate
// and steal each other's staging file). Both must be zero.
func TestConcurrentEnableLensLosesNothing(t *testing.T) {
	const trials, writers = 20, 6
	for trial := 0; trial < trials; trial++ {
		s := tempStore(t)
		var wg sync.WaitGroup
		errs := make([]error, writers)
		for i := 0; i < writers; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				errs[i] = s.EnableLens(fmt.Sprintf("lens%d", i))
			}(i)
		}
		wg.Wait()

		enabled := map[string]bool{}
		for _, n := range s.LoadConfig().EnabledLenses {
			enabled[n] = true
		}
		for i := 0; i < writers; i++ {
			name := fmt.Sprintf("lens%d", i)
			if errs[i] != nil {
				// A rename/tmp error means writers destroyed each other's staging file.
				if strings.Contains(errs[i].Error(), ".tmp") || strings.Contains(errs[i].Error(), "no such file") {
					t.Fatalf("trial %d: writer %d hit a staging-file collision: %v", trial, i, errs[i])
				}
				t.Fatalf("trial %d: writer %d: %v", trial, i, errs[i])
			}
			// Reported success, so the line MUST be there.
			if !enabled[name] {
				t.Fatalf("trial %d: EnableLens(%q) returned nil but the lens is not enabled — silent lost update (enabled=%v)",
					trial, name, s.LoadConfig().EnabledLenses)
			}
		}
	}
}

// writeAtomic must stage through a UNIQUE temp file. With a shared path+".tmp", one
// writer's os.WriteFile truncates another's half-written file and the loser's rename
// ENOENTs — surfacing from `witness config set` as what reads like a corrupt archive.
func TestWriteAtomicConcurrentWritersDoNotCollide(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/target.txt"
	const n = 12
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = writeAtomic(path, []byte(strings.Repeat(fmt.Sprintf("%d", i), 4096)))
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Errorf("writer %d: %v", i, err)
		}
	}
	// Last-writer-wins is fine; a TORN file is not. Every byte must come from one writer.
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	got := string(raw)
	if len(got) == 0 {
		t.Fatal("target is empty")
	}
	first := got[:1]
	if strings.Trim(got, first) != "" {
		t.Fatalf("torn write: file mixes content from multiple writers (starts %q, len %d)", first, len(got))
	}
}
