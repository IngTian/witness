package main

// Can two concurrent writeAtomic calls on the SHARED "<path>.tmp" publish a TRUNCATED
// (partial) file? Writer A: huge payload (slow write). Writer B: tiny payload.
// A torn publish = config.toml whose length is neither len(A) nor len(B).

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
	dir, _ := os.MkdirTemp("", "tear")
	path := dir + "/config.toml"
	big := []byte(strings.Repeat("A", 64<<20)) // 64MB: a multi-ms write
	small := []byte(strings.Repeat("b", 4096))
	torn, renameErr, empty := 0, 0, 0
	for round := 0; round < 200; round++ {
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			if err := writeAtomic(path, big); err != nil {
				renameErr++
			}
		}()
		go func() {
			defer wg.Done()
			if err := writeAtomic(path, small); err != nil {
				renameErr++
			}
		}()
		wg.Wait()
		fi, err := os.Stat(path)
		if err != nil {
			fmt.Println("MISSING config.toml:", err)
			continue
		}
		switch {
		case fi.Size() == 0:
			empty++
		case fi.Size() != int64(len(big)) && fi.Size() != int64(len(small)):
			torn++
			fmt.Printf("round %d TORN size=%d\n", round, fi.Size())
		}
	}
	fmt.Printf("rounds=200 torn=%d empty=%d writeAtomicErrors=%d\n", torn, empty, renameErr)
	os.RemoveAll(dir)
}
