package main

// For each archive dir given on argv: report whether config.toml has an active
// `runner =` line and what the DB's runner_bound flag says, plus the effective
// ResolveRunner under WITNESS_RUNNER=opencode (the npm plugin fallback).

import (
	"fmt"
	"os"
	"strings"

	"github.com/IngTian/witness/internal/store"
)

func main() {
	os.Setenv("WITNESS_NO_DEFAULT_LENS", "1")
	os.Setenv("WITNESS_RUNNER", "opencode")
	bad := 0
	for _, dir := range os.Args[1:] {
		os.Setenv("WITNESS_HOME", dir)
		s, err := store.Open()
		if err != nil {
			continue
		}
		data, _ := os.ReadFile(s.ConfigPath())
		hasLine := false
		lensLines := 0
		for _, l := range strings.Split(string(data), "\n") {
			t := strings.TrimSpace(l)
			if t == "" || strings.HasPrefix(t, "#") {
				continue
			}
			k, _, ok := strings.Cut(t, "=")
			if !ok {
				continue
			}
			switch strings.TrimSpace(k) {
			case "runner":
				hasLine = true
			case "lens":
				lensLines++
			}
		}
		bound := s.MetaString("runner_bound")
		resolved := s.ResolveRunner(s.LoadConfig())
		if bound == "1" && !hasLine {
			bad++
			fmt.Printf("%s: BOUND-BUT-NO-LINE  resolve=%q lensLines=%d size=%d\n", dir, resolved, lensLines, len(data))
		}
		s.Close()
	}
	fmt.Println("archives with runner_bound=1 and NO runner line:", bad)
}
