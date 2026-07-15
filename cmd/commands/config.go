package commands

import (
	"fmt"
	"sort"
	"strings"

	"github.com/IngTian/witness/internal/store"
	"github.com/spf13/cobra"
)

// configKeys is the allowlist `witness config` can get/set — the string knobs a user
// actually needs to tune the distillation backend without hand-editing config.toml.
// Restricting to these keeps the CLI honest: it never writes a key LoadConfig can't
// read, and the descriptions double as the command's help. Non-string tunables
// (review_every, mine_concurrency, …) stay config.toml-only for now — they're rarely
// changed and adding typed validation here isn't worth it yet.
var configKeys = map[string]string{
	"runner":        "distillation runtime: `claude` or `opencode` (also settable via `witness install`)",
	"triage_model":  "model for per-session mining (empty = the runner's environment default)",
	"distill_model": "model for the reviewer + profile summarizer (empty = the runner's default)",
}

func newConfigCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "config",
		Short: "Get or set distillation config (runner, models).",
		Long: "Get or set the string config.toml knobs that control the distillation backend, without hand-editing the file:\n\n" +
			configKeyHelp() +
			"\n\nThe runner is GLOBAL — one backend distills every session and every lens; there is no per-lens model today. A lens that needs a strong model just means: point the global runner at a capable one.",
	}
	c.AddCommand(
		&cobra.Command{
			Use:   "get [key]",
			Short: "Print one config value, or all with no key.",
			Args:  cobra.MaximumNArgs(1),
			RunE:  func(_ *cobra.Command, args []string) error { return cmdConfigGet(args) },
		},
		&cobra.Command{
			Use:   "set <key> <value>",
			Short: "Set a config value (use \"\" to clear a model back to the runner default).",
			Args:  cobra.ExactArgs(2),
			RunE:  func(_ *cobra.Command, args []string) error { return cmdConfigSet(args[0], args[1]) },
		},
		&cobra.Command{
			Use:   "path",
			Short: "Print the config.toml path.",
			Args:  cobra.NoArgs,
			RunE:  func(_ *cobra.Command, _ []string) error { return cmdConfigPath() },
		},
	)
	return c
}

func configKeyHelp() string {
	keys := sortedConfigKeys()
	var b strings.Builder
	for _, k := range keys {
		fmt.Fprintf(&b, "  %-14s %s\n", k, configKeys[k])
	}
	return strings.TrimRight(b.String(), "\n")
}

func sortedConfigKeys() []string {
	keys := make([]string, 0, len(configKeys))
	for k := range configKeys {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// currentConfigValue returns the effective value of a config key as LoadConfig sees
// it, so `config get` reflects what the engine will actually use (not just the raw
// file bytes). Runner routes through ResolveRunner so a WITNESS_RUNNER env fallback is
// visible too — matching what `doctor` and the worker resolve.
func currentConfigValue(st *store.Store, key string) string {
	cfg := st.LoadConfig()
	switch key {
	case "runner":
		return st.ResolveRunner(cfg)
	case "triage_model":
		return cfg.TriageModel
	case "distill_model":
		return cfg.DistillModel
	default:
		return ""
	}
}

func cmdConfigGet(args []string) error {
	st, err := store.Open()
	if err != nil {
		return err
	}
	defer st.Close()
	if len(args) == 1 {
		key := args[0]
		if _, ok := configKeys[key]; !ok {
			return fmt.Errorf("unknown config key %q (want: %s)", key, strings.Join(sortedConfigKeys(), ", "))
		}
		v := currentConfigValue(st, key)
		if v == "" {
			v = "(runner default)"
		}
		fmt.Println(v)
		return nil
	}
	// No key: print all, aligned, with the default marker for empty model fields.
	for _, k := range sortedConfigKeys() {
		v := currentConfigValue(st, k)
		if v == "" {
			v = dim("(runner default)")
		}
		fmt.Printf("%-14s %s\n", k, v)
	}
	return nil
}

func cmdConfigSet(key, value string) error {
	if _, ok := configKeys[key]; !ok {
		return fmt.Errorf("unknown config key %q (want: %s)", key, strings.Join(sortedConfigKeys(), ", "))
	}
	value = strings.TrimSpace(value)
	if key == "runner" && value != store.RunnerClaude && value != store.RunnerOpenCode {
		return fmt.Errorf("runner must be %q or %q, got %q", store.RunnerClaude, store.RunnerOpenCode, value)
	}
	st, err := store.Open()
	if err != nil {
		return err
	}
	defer st.Close()
	if err := st.SetConfigString(key, value); err != nil {
		return fmt.Errorf("set %s: %w", key, err)
	}
	shown := value
	if shown == "" {
		shown = "(runner default)"
	}
	fmt.Printf("set %s = %s\n", key, shown)
	return nil
}

func cmdConfigPath() error {
	st, err := store.Open()
	if err != nil {
		return err
	}
	defer st.Close()
	fmt.Println(st.ConfigPath())
	return nil
}
