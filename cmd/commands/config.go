package commands

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/IngTian/witness/internal/lens"
	"github.com/IngTian/witness/internal/platform"
	"github.com/IngTian/witness/internal/store"
	"github.com/spf13/cobra"
)

// canonicalConfigKey maps a user-typed key (incl. legacy synonyms) to the canonical
// key, or "" if unknown.
func canonicalConfigKey(k string) string {
	switch strings.ToLower(strings.TrimSpace(k)) {
	case "runner":
		return "runner"
	case "mine_model", "triage_model", "extract_model":
		return "mine_model"
	case "review_model", "distill_model":
		return "review_model"
	}
	return ""
}

func newConfigCmd() *cobra.Command {
	c := &cobra.Command{
		Use:     "config",
		GroupID: groupConfig,
		Short:   "Get or set config (runner, models) — default scope or per-lens.",
		Long: `Get or set config knobs that control distillation: runner, mine_model (mining / triage), review_model (review).

The config tree has TWO scopes:
  • DEFAULT scope (no --lens): the baseline runner + models every lens rides unless it overrides.
  • PER-LENS scope (--lens <name>): a lens's runner/model overrides; empty = inherit from default.

Canonical keys:
  runner        distillation runtime: "claude" or "opencode" (also settable via ` + "`witness install`" + `)
  mine_model    MINING model — L0 turns → L1 observations, per session (frequent, dominant cost). Empty = the runner's default.
  review_model  REVIEW model — L1 observations → L2 facets + L4 profile, batched. Empty = falls back to mine_model, so one setting covers the whole pipeline.

Legacy synonyms (accepted in get/set):
  triage_model → mine_model, distill_model → review_model, extract_model → mine_model.

Precedence: per-lens › default › environment.`,
	}
	var lensName string
	getCmd := &cobra.Command{
		Use:   "get [key]",
		Short: "Print one config value, or all with no key.",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			return cmdConfigGet(args, lensName)
		},
	}
	getCmd.Flags().StringVar(&lensName, "lens", "", "scope to a lens (show its overrides + inherited values)")

	setCmd := &cobra.Command{
		Use:   "set <key> <value>",
		Short: "Set a config value (use \"\" to clear back to the default).",
		Args:  cobra.ExactArgs(2),
		RunE: func(_ *cobra.Command, args []string) error {
			return cmdConfigSet(args[0], args[1], lensName)
		},
	}
	setCmd.Flags().StringVar(&lensName, "lens", "", "scope to a lens (set a per-lens override)")

	unsetCmd := &cobra.Command{
		Use:   "unset <key>",
		Short: "Clear a config key (for a lens, makes it inherit from default).",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			return cmdConfigSet(args[0], "", lensName)
		},
	}
	unsetCmd.Flags().StringVar(&lensName, "lens", "", "scope to a lens (clear its override → inherit from default)")

	listCmd := &cobra.Command{
		Use:   "list",
		Short: "List all config keys with their values and origin.",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			return cmdConfigList(lensName)
		},
	}
	listCmd.Flags().StringVar(&lensName, "lens", "", "scope to a lens (show per-lens overrides + inherited defaults)")

	c.AddCommand(getCmd, setCmd, unsetCmd, listCmd,
		&cobra.Command{
			Use:   "path",
			Short: "Print the config.toml path.",
			Args:  cobra.NoArgs,
			RunE:  func(_ *cobra.Command, _ []string) error { return cmdConfigPath() },
		})
	return c
}

func cmdConfigGet(args []string, lensName string) error {
	st, err := store.Open()
	if err != nil {
		return err
	}
	defer st.Close()
	if len(args) == 1 {
		key := canonicalConfigKey(args[0])
		if key == "" {
			return fmt.Errorf("unknown config key %q (want: runner, mine_model, review_model)", args[0])
		}
		v, err := configReadValue(st, key, lensName)
		if err != nil {
			return err
		}
		if v == "" {
			if lensName != "" {
				v = "(inherited from default)"
			} else {
				v = "(runner default)"
			}
		}
		fmt.Println(v)
		return nil
	}
	// No key: print all, aligned, with the default marker for empty fields.
	return cmdConfigList(lensName)
}

func cmdConfigSet(key, value string, lensName string) error {
	canonical := canonicalConfigKey(key)
	if canonical == "" {
		return fmt.Errorf("unknown config key %q (want: runner, mine_model, review_model)", key)
	}
	value = strings.TrimSpace(value)
	// runner validation ONLY at default scope; at lens scope allow any runner string
	// (a lens may target a runtime that isn't the default — matches today's `lens set --runner`).
	if canonical == "runner" && lensName == "" && value != "" {
		// Ask the platform registry, not a hardcoded pair. The old allowlist was a closed
		// set in a system that is otherwise uniformly registry-driven, and it DISAGREED with
		// the other writer of this same key: `witness install <target>` → bindRunner writes
		// it with no validation at all. Both now go through platform.ValidateRunnerName, so a
		// newly registered runtime is accepted without editing this file.
		if err := platform.ValidateRunnerName(value); err != nil {
			return err
		}
	}
	st, err := store.Open()
	if err != nil {
		return err
	}
	defer st.Close()
	if err := configApplySet(st, canonical, value, lensName); err != nil {
		return err
	}
	shown := value
	if shown == "" {
		switch {
		case lensName != "":
			shown = "(cleared — inherits from default)"
		case canonical == "runner":
			// Say what actually happened. "(runner default)" implied a value had been chosen,
			// while the effect is the opposite: the key is gone and resolution falls back.
			shown = "(cleared — unbound; resolves from WITNESS_RUNNER, else the built-in default)"
		default:
			shown = "(runner default)"
		}
	}
	scope := "default"
	if lensName != "" {
		scope = fmt.Sprintf("lens %q", lensName)
	}
	fmt.Printf("set %s · %s = %s\n", scope, canonical, shown)
	return nil
}

func cmdConfigList(lensName string) error {
	st, err := store.Open()
	if err != nil {
		return err
	}
	defer st.Close()

	if lensName == "" {
		// DEFAULT scope: render the three default keys.
		cfg := st.LoadConfig()
		runner := st.ResolveRunner(cfg)
		fmt.Println(header("config · default"))
		fmt.Println()
		fmt.Println(kvRow("runner", runner, "default"))
		mine := cfg.TriageModel
		if mine == "" {
			mine = dim("(runner default)")
		}
		fmt.Println(kvRow("mine_model", mine, "default"))
		review := cfg.DistillModel
		if review == "" {
			review = dim("(runner default)")
		}
		fmt.Println(kvRow("review_model", review, "default"))
		fmt.Println()
		fmt.Println(footer("precedence: per-lens › default › environment"))
		return nil
	}

	// LENS scope: render the lens's overrides + inherited values with origin notes.
	if err := configCheckLensExists(st, lensName); err != nil {
		return err
	}
	// Read the RAW lens.json to know override vs. inherited (LoadRegistered resolves inherited values).
	rawCfg, err := configReadRawLensJSON(st, lensName)
	if err != nil {
		return err
	}
	// Also load the resolved lens to get the effective values (including inherited ones).
	l, err := lens.LoadRegistered(lensName, st.LensesDir())
	if err != nil {
		return fmt.Errorf("load lens %q: %w", lensName, err)
	}
	fmt.Printf("%s\n", header(fmt.Sprintf("config · lens: %s", lensName)))
	fmt.Println()
	// runner
	runnerOrigin := "inherited from default"
	if strings.TrimSpace(rawCfg.Runner) != "" {
		runnerOrigin = "lens override"
	}
	runnerVal := l.Runner
	if runnerVal == "" {
		// Resolved to empty — need to show the default runner value, not blank.
		runnerVal = st.ResolveRunner(st.LoadConfig())
	}
	fmt.Println(kvRow("runner", runnerVal, runnerOrigin))
	// mine_model (extract)
	mineOrigin := "inherited from default"
	if strings.TrimSpace(rawCfg.ExtractModel) != "" {
		mineOrigin = "lens override"
	}
	mineVal := l.ExtractModel
	if mineVal == "" {
		cfg := st.LoadConfig()
		mineVal = cfg.TriageModel
		if mineVal == "" {
			mineVal = dim("(runner default)")
		}
	}
	fmt.Println(kvRow("mine_model", mineVal, mineOrigin))
	// review_model
	reviewOrigin := "inherited from default"
	if strings.TrimSpace(rawCfg.ReviewModel) != "" {
		reviewOrigin = "lens override"
	}
	reviewVal := l.ReviewModel
	if reviewVal == "" {
		cfg := st.LoadConfig()
		reviewVal = cfg.DistillModel
		if reviewVal == "" {
			reviewVal = dim("(runner default)")
		}
	}
	fmt.Println(kvRow("review_model", reviewVal, reviewOrigin))
	fmt.Println()
	fmt.Println(footer("precedence: per-lens › default › environment"))
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

// configApplySet is the internal helper the `set` RunE calls — extracted so it's unit-testable
// without cobra. lensName="" means default scope; non-empty means lens scope.
func configApplySet(st *store.Store, key, value, lensName string) error {
	if lensName == "" {
		// DEFAULT scope
		switch key {
		case "runner":
			if value == "" {
				// Clearing the runner must UNBIND it, not bind it to the template default.
				// st.SetRunner("") wrote a line AND stamped runner_bound=1, so `config unset
				// runner` did the opposite of what it says: measured on a fresh archive with
				// WITNESS_RUNNER=opencode, resolution went from "opencode" to a BOUND "claude",
				// and no env change could recover it.
				return st.UnsetRunner()
			}
			return st.SetRunner(value)
		case "mine_model":
			return st.SetConfigString("triage_model", value)
		case "review_model":
			return st.SetConfigString("distill_model", value)
		default:
			return fmt.Errorf("unknown config key %q", key)
		}
	}
	// LENS scope
	if err := configCheckLensExists(st, lensName); err != nil {
		return err
	}
	switch key {
	case "runner":
		return st.SetLensRunner(lensName, value)
	case "mine_model":
		return st.SetLensModel(lensName, "extract", value)
	case "review_model":
		return st.SetLensModel(lensName, "review", value)
	default:
		return fmt.Errorf("unknown config key %q", key)
	}
}

// configReadValue reads the effective value of a config key for the given scope.
// lensName="" means default scope; non-empty means lens scope (returns the resolved value,
// which may be inherited from the default).
func configReadValue(st *store.Store, key, lensName string) (string, error) {
	if lensName == "" {
		// DEFAULT scope
		cfg := st.LoadConfig()
		switch key {
		case "runner":
			return st.ResolveRunner(cfg), nil
		case "mine_model":
			return cfg.TriageModel, nil
		case "review_model":
			return cfg.DistillModel, nil
		default:
			return "", fmt.Errorf("unknown config key %q", key)
		}
	}
	// LENS scope
	if err := configCheckLensExists(st, lensName); err != nil {
		return "", err
	}
	l, err := lens.LoadRegistered(lensName, st.LensesDir())
	if err != nil {
		return "", fmt.Errorf("load lens %q: %w", lensName, err)
	}
	switch key {
	case "runner":
		if l.Runner != "" {
			return l.Runner, nil
		}
		return st.ResolveRunner(st.LoadConfig()), nil
	case "mine_model":
		if l.ExtractModel != "" {
			return l.ExtractModel, nil
		}
		return st.LoadConfig().TriageModel, nil
	case "review_model":
		if l.ReviewModel != "" {
			return l.ReviewModel, nil
		}
		return st.LoadConfig().DistillModel, nil
	default:
		return "", fmt.Errorf("unknown config key %q", key)
	}
}

// configCheckLensExists returns an error if the lens is not registered.
func configCheckLensExists(st *store.Store, name string) error {
	registered := st.RegisteredLenses()
	for _, n := range registered {
		if n == name {
			return nil
		}
	}
	return fmt.Errorf("lens %q is not registered (see `witness lens list`)", name)
}

// configReadRawLensJSON reads the raw lens.json (the on-disk struct) to distinguish
// an override from an inherited value. lens.LoadRegistered resolves inherited values,
// so it can't tell which fields are overrides — but the origin view needs that.
// This is a READ of a file the CLI already owns the path to — NOT an engine change.
func configReadRawLensJSON(st *store.Store, name string) (lens.LensConfig, error) {
	path := filepath.Join(st.LensesDir(), name, "lens.json")
	var cfg lens.LensConfig
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			// No lens.json means all fields ride the default (no overrides).
			return cfg, nil
		}
		return cfg, fmt.Errorf("read lens.json: %w", err)
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return cfg, fmt.Errorf("parse lens.json: %w", err)
	}
	return cfg, nil
}
