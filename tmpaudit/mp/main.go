package main

// Multi-PROCESS proof of the unlocked config.toml read-modify-write.
// Parent: seeds an archive, then forks N children that each EnableLens("lensK")
// concurrently as separate OS processes; then reports which children reported
// SUCCESS (exit 0) yet whose lens is absent from config.toml — silent data loss.

import (
	"fmt"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"sync"

	"github.com/IngTian/witness/internal/store"
)

func main() {
	if k := os.Getenv("CHILD_LENS"); k != "" {
		s, err := store.Open()
		if err != nil {
			os.Exit(2)
		}
		defer s.Close()
		if err := s.EnableLens(k); err != nil {
			os.Exit(3) // reported a failure — NOT silent
		}
		return // reported SUCCESS
	}
	home := os.Getenv("WITNESS_HOME")
	s, err := store.Open()
	if err != nil {
		panic(err)
	}
	s.Close()
	self, _ := os.Executable()
	n := 8
	silentLoss, reportedFail := 0, 0
	rounds := 20
	for r := 0; r < rounds; r++ {
		os.RemoveAll(home)
		s0, _ := store.Open()
		s0.Close()
		ok := make([]bool, n)
		var wg sync.WaitGroup
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				c := exec.Command(self)
				c.Env = append(os.Environ(), "CHILD_LENS=lens"+strconv.Itoa(i), "WITNESS_HOME="+home, "WITNESS_NO_DEFAULT_LENS=1")
				ok[i] = c.Run() == nil
			}(i)
		}
		wg.Wait()
		s2, _ := store.Open()
		enabled := s2.LoadConfig().EnabledLenses
		s2.Close()
		for i := 0; i < n; i++ {
			name := "lens" + strconv.Itoa(i)
			switch {
			case !ok[i]:
				reportedFail++
			case !slices.Contains(enabled, name):
				silentLoss++
			}
		}
	}
	fmt.Printf("rounds=%d writers=%d  SILENT LOSS (child exited 0, lens absent)=%d  loud failures=%d  total=%d\n",
		rounds, n, silentLoss, reportedFail, rounds*n)
}
