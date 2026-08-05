package main

// Precise hunt for the PARTIAL/EMPTY publish through the shared "<path>.tmp".
// Writer B's payload is huge so its OpenFile(O_TRUNC)->write window is wide;
// writer A is tiny and races its rename into that window. Record the exact
// published sizes to see if anything other than len(A)/len(B) is ever visible.

import (
	"fmt"
	"os"
	"strings"
	"sync"
	"time"
)

func writeAtomic(path string, data []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func main() {
	dir, _ := os.MkdirTemp("", "tear3")
	path := dir + "/config.toml"
	bigN := 256 << 20 // 256MB
	big := []byte(strings.Repeat("A", bigN))
	small := []byte(strings.Repeat("b", 3200)) // ~ the real config.toml template size
	sizes := map[int64]int{}
	for round := 0; round < 60; round++ {
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); _ = writeAtomic(path, big) }()
		go func() {
			defer wg.Done()
			time.Sleep(time.Duration(round%7) * time.Millisecond) // slide into the window
			_ = writeAtomic(path, small)
		}()
		wg.Wait()
		fi, err := os.Stat(path)
		if err != nil {
			sizes[-1]++
			continue
		}
		sizes[fi.Size()]++
	}
	fmt.Printf("len(big)=%d len(small)=%d\npublished sizes: %v\n", len(big), len(small), sizes)
	os.RemoveAll(dir)
}
