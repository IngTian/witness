package main

import (
	"fmt"
	"os"

	"github.com/IngTian/witness/internal/store"
)

func main() {
	for _, d := range os.Args[1:] {
		os.Setenv("WITNESS_HOME", d)
		s, err := store.Open()
		if err != nil {
			continue
		}
		fmt.Printf("%s\n  default_lens_migrated_v1a=%q runner_bound=%q enabled=%v registered=%v\n",
			d, s.MetaString("default_lens_migrated_v1a"), s.MetaString("runner_bound"),
			s.LoadConfig().EnabledLenses, s.RegisteredLenses())
		s.Close()
	}
}
