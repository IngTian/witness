package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/IngTian/witness/internal/store"
)

func main() {
	root := os.Getenv("WITNESS_HOME")
	s, err := store.Open()
	if err != nil {
		fmt.Println("open err:", err)
		return
	}
	cfgPath := filepath.Join(root, "config.toml")
	if err := s.SetRunner("opencode"); err != nil {
		fmt.Println("setrunner:", err)
	}
	if err := s.EnableLens("math"); err != nil {
		fmt.Println("enablelens:", err)
	}
	c := s.LoadConfig()
	fmt.Printf("READABLE: runner=%q resolved=%q enabled=%v bound=%q\n", c.Runner, s.ResolveRunner(c), c.EnabledLenses, s.MetaString("runner_bound"))
	s.Close()

	if err := os.Chmod(cfgPath, 0o000); err != nil {
		fmt.Println("chmod:", err)
		return
	}
	s2, err := store.Open()
	if err != nil {
		fmt.Println("open2 err:", err)
		return
	}
	defer s2.Close()
	c2 := s2.LoadConfig()
	fmt.Printf("UNREADABLE: runner=%q resolved=%q enabled=%v bound=%q\n", c2.Runner, s2.ResolveRunner(c2), c2.EnabledLenses, s2.MetaString("runner_bound"))
	fmt.Println("SetConfigString err =", s2.SetConfigString("triage_model", "x"))
	b, rerr := os.ReadFile(cfgPath)
	fmt.Println("direct read err =", rerr, "len", len(b))
	pend, perr := s2.PendingSessions(c2.EnabledLenses)
	fmt.Println("pending with that lens set:", pend, perr)
	fmt.Println("uid:", os.Getuid())
	_ = os.Chmod(cfgPath, 0o600)
}
