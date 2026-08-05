package main

// Build an archive with unmined L0 and a 0-byte config.toml (the torn-publish outcome),
// so we can see what `witness doctor` reports.

import (
	"fmt"
	"os"
	"time"

	"github.com/IngTian/witness/internal/store"
)

func main() {
	s, err := store.Open()
	if err != nil {
		panic(err)
	}
	for i := 0; i < 40; i++ {
		if err := s.AppendRaw(store.RawRecord{
			TS: time.Now().UTC().Format(time.RFC3339), Session: "sess-A", Seq: i,
			Role: "user", Text: "a long user turn about refactoring the queue",
		}); err != nil {
			panic(err)
		}
	}
	_ = s.EnableLens("default")
	fmt.Println("enabled:", s.LoadConfig().EnabledLenses)
	// The torn publish: config.toml becomes 0 bytes.
	if os.Getenv("TEAR") == "1" {
		if err := os.WriteFile(s.ConfigPath(), nil, 0o600); err != nil {
			panic(err)
		}
		fmt.Println("config.toml truncated to 0 bytes")
	}
	s.Close()
}
