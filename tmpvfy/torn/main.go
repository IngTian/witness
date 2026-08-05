package main

// Does the SHARED "<path>.tmp" let one process publish ANOTHER process's
// partially-written bytes? Writers use LONG values; a reader loops and validates
// every published config.toml: the triage_model line must be a complete quoted run
// of 4000 identical chars, and the file must never be empty.
import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/IngTian/witness/internal/store"
)

func validate(s string) (bad bool, why string) {
	if len(s) == 0 {
		return true, "EMPTY FILE"
	}
	for _, line := range strings.Split(s, "\n") {
		t := strings.TrimSpace(line)
		if !strings.HasPrefix(t, "triage_model") {
			continue
		}
		_, v, _ := strings.Cut(t, "=")
		v = strings.TrimSpace(v)
		if v == `""` {
			return false, ""
		}
		if len(v) < 2 || v[0] != '"' || v[len(v)-1] != '"' {
			return true, fmt.Sprintf("unterminated value len=%d head=%q", len(v), v[:min(30, len(v))])
		}
		inner := v[1 : len(v)-1]
		if len(inner) != 4000 {
			return true, fmt.Sprintf("value len=%d (want 4000)", len(inner))
		}
		for i := 0; i < len(inner); i++ {
			if inner[i] != inner[0] {
				return true, "mixed chars in value"
			}
		}
	}
	// no triage_model line at all after a writer ran = also suspicious, but the
	// template is valid before any write, so don't flag it.
	return false, ""
}

func main() {
	root := os.Getenv("W_ROOT")
	switch os.Getenv("W_ROLE") {
	case "child":
		st, err := store.Open()
		if err != nil {
			fmt.Println("openerr:", err)
			return
		}
		defer st.Close()
		val := strings.Repeat(os.Getenv("W_CH"), 4000)
		for i := 0; i < 600; i++ {
			if err := st.SetConfigString("triage_model", val); err != nil {
				fmt.Println("ERR:", err)
			}
		}
		return
	case "reader":
		p := root + "/config.toml"
		var bad, n int
		deadline := time.Now().Add(8 * time.Second)
		for time.Now().Before(deadline) {
			data, err := os.ReadFile(p)
			if err != nil {
				fmt.Println("READ ERR:", err)
				continue
			}
			n++
			if isBad, why := validate(string(data)); isBad {
				bad++
				if bad < 6 {
					fmt.Printf("CORRUPT read len=%d: %s\n", len(data), why)
				}
			}
		}
		fmt.Printf("reader: reads=%d corrupt=%d\n", n, bad)
		return
	}
	self, _ := os.Executable()
	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			role := "child"
			if i == 4 {
				role = "reader"
			}
			c := exec.Command(self)
			c.Env = append(os.Environ(), "W_ROLE="+role, fmt.Sprintf("W_CH=%c", 'a'+i), "WITNESS_HOME="+root)
			out, _ := c.CombinedOutput()
			errs := 0
			for _, l := range strings.Split(strings.TrimSpace(string(out)), "\n") {
				if strings.HasPrefix(l, "ERR:") {
					errs++
					continue
				}
				if l != "" {
					fmt.Println(role, "|", l)
				}
			}
			if errs > 0 {
				fmt.Printf("%s | writer rename errors=%d\n", role, errs)
			}
		}(i)
	}
	wg.Wait()
	data, err := os.ReadFile(root + "/config.toml")
	isBad, why := validate(string(data))
	fmt.Printf("FINAL: len=%d err=%v corrupt=%v (%s)\n", len(data), err, isBad, why)
}
