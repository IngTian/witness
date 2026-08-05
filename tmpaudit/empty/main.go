package main

// Brute-force the ZERO-BYTE publish through the shared "<path>.tmp":
//   A: WriteFile(tmp,data) returns  ->  B: OpenFile(tmp, O_TRUNC) truncates it
//   ->  A: Rename(tmp,path) publishes a 0-byte (or partial) config.toml.
// A concurrent READER samples the published file the whole time.

import (
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
)

func writeAtomic(path string, data []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func main() {
	dir, _ := os.MkdirTemp("", "empty")
	defer os.RemoveAll(dir)
	path := dir + "/config.toml"
	payload := []byte(strings.Repeat("lens = default\n", 60)) // ~900B, config-sized
	var stop atomic.Bool
	var reads, badReads int64
	var wg sync.WaitGroup
	// Reader: what other witness processes' LoadConfig would see.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for !stop.Load() {
			b, err := os.ReadFile(path)
			if err != nil {
				continue
			}
			atomic.AddInt64(&reads, 1)
			if len(b) != len(payload) {
				atomic.AddInt64(&badReads, 1)
				if badReads < 6 {
					fmt.Printf("BAD READ: size=%d (expected %d)\n", len(b), len(payload))
				}
			}
		}
	}()
	writers := 32
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 30000; i++ {
				_ = writeAtomic(path, payload)
			}
		}()
	}
	// let the writers finish
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	<-done
	stop.Store(true)
	fmt.Printf("writers=%d writes=%d  reads=%d  ANOMALOUS reads=%d\n", writers, writers*30000, reads, badReads)
}
