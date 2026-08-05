package main

// Multi-PROCESS reproduction: N child processes each call the real store API
// writers (EnableLens / SetConfigString) against ONE archive root.
import (
	"fmt"
	"os"
	"os/exec"
	"sync"

	"github.com/IngTian/witness/internal/store"
)

func main() {
	root := os.Getenv("W_ROOT")
	if role := os.Getenv("W_ROLE"); role == "child" {
		st, err := store.Open()
		if err != nil {
			fmt.Println("openerr:", err)
			return
		}
		defer st.Close()
		k := os.Getenv("W_KEY")
		for i := 0; i < 200; i++ {
			var err error
			if k == "lens" {
				err = st.EnableLens(os.Getenv("W_NAME"))
			} else {
				err = st.SetConfigString("triage_model", os.Getenv("W_NAME"))
			}
			if err != nil {
				fmt.Println("ERR:", err)
			}
		}
		return
	}
	// parent
	self, _ := os.Executable()
	var wg sync.WaitGroup
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			c := exec.Command(self)
			key := "cfg"
			if i%2 == 0 {
				key = "lens"
			}
			c.Env = append(os.Environ(), "W_ROLE=child", "W_KEY="+key, fmt.Sprintf("W_NAME=lens%d", i), "WITNESS_HOME="+root)
			out, _ := c.CombinedOutput()
			os.Stdout.Write(out)
		}(i)
	}
	wg.Wait()
	data, err := os.ReadFile(root + "/config.toml")
	fmt.Printf("=== final config.toml (err=%v) len=%d ===\n%s\n", err, len(data), string(data))
}
