package main

// Quantify the SHARED-tmp collision through the REAL store write path: how often does a
// concurrent config.toml writer fail with `rename ... no such file or directory` because
// a PEER renamed the shared "<path>.tmp" out from under it?

import (
	"fmt"
	"os"
	"strings"
	"sync"

	"github.com/IngTian/witness/internal/store"
)

func main() {
	os.Setenv("WITNESS_NO_DEFAULT_LENS", "1")
	dir, _ := os.MkdirTemp("", "enoent")
	defer os.RemoveAll(dir)
	os.Setenv("WITNESS_HOME", dir)
	s, err := store.Open()
	if err != nil {
		panic(err)
	}
	defer s.Close()

	enoent, other, total := 0, 0, 0
	var mu sync.Mutex
	for round := 0; round < 300; round++ {
		var wg sync.WaitGroup
		for i := 0; i < 6; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				err := s.SetConfigString("triage_model", fmt.Sprint(i))
				mu.Lock()
				defer mu.Unlock()
				total++
				switch {
				case err == nil:
				case strings.Contains(err.Error(), "no such file or directory"):
					enoent++
				default:
					other++
				}
			}(i)
		}
		wg.Wait()
	}
	fmt.Printf("config writes=%d  failed with rename-ENOENT (shared tmp stolen)=%d  other errors=%d\n",
		total, enoent, other)
}
