package main

// Hunt the EMPTY/PARTIAL publish through the shared "<path>.tmp":
// A: WriteFile(tmp) done -> B: OpenFile(tmp, O_TRUNC) truncates it -> A: Rename publishes
// a 0-byte (or partial) config.toml. Many writers with a payload big enough that the
// write straddles the other's rename.

import (
	"fmt"
	"os"
	"strings"
	"sync"
)

func writeAtomic(path string, data []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func main() {
	dir, _ := os.MkdirTemp("", "tear2")
	path := dir + "/config.toml"
	payload := []byte(strings.Repeat("x", 3<<20)) // 3MB
	n := 16
	bad := map[int64]int{}
	for round := 0; round < 500; round++ {
		var wg sync.WaitGroup
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func() { defer wg.Done(); _ = writeAtomic(path, payload) }()
		}
		wg.Wait()
		fi, err := os.Stat(path)
		if err != nil {
			bad[-1]++
			continue
		}
		if fi.Size() != int64(len(payload)) {
			bad[fi.Size()]++
		}
	}
	fmt.Printf("rounds=500 x %d writers; anomalous published sizes: %v\n", n, bad)
	os.RemoveAll(dir)
}
