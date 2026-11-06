package commands

import (
	"fmt"
	"strings"

	"github.com/IngTian/witness/internal/distill"
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

// setting is one config key as the engine will actually use it: value "" means the runner's own
// default, and from says where the value came from ("" when there is nothing worth saying).
type setting struct{ key, value, from string }

// effectiveConfig resolves runner, mine_model and review_model the way the engine does —
// ResolveRunner, then distill.RunnerFor / distill.ModelFor — so every surface shows what will run,
// not the raw field. Reading the raw field said review_model was "(runner default)" while review
// actually ran on mine_model, because an empty review_model falls back to it. l == nil is the
// default scope; otherwise l is the lens as loaded from the registry (its fields are the overrides).
func effectiveConfig(st *store.Store, l *lens.Lens) []setting {
	cfg := st.ResolvedConfig()
	runnerFrom := st.RunnerOrigin()
	if l == nil {
		mineFrom, reviewFrom := "", ""
		if cfg.TriageModel != "" {
			mineFrom = "config.toml"
		}
		switch {
		case cfg.DistillModel != "":
			reviewFrom = "config.toml"
		case cfg.TriageModel != "":
			reviewFrom = "falls back to mine_model"
		}
		return []setting{
			{"runner", cfg.Runner, runnerFrom},
			{"mine_model", distill.ModelFor(cfg, nil, distill.PhaseExtract), mineFrom},
			{"review_model", distill.ModelFor(cfg, nil, distill.PhaseReview), reviewFrom},
		}
	}
	runner, from := distill.RunnerFor(cfg, l), "inherited from default"
	if l.Runner != "" {
		from = "lens override"
	}
	out := []setting{{"runner", runner, from}}
	for _, m := range []struct {
		key, override string
		phase         distill.Phase
	}{{"mine_model", l.ExtractModel, distill.PhaseExtract}, {"review_model", l.ReviewModel, distill.PhaseReview}} {
		from := "inherited from default"
		switch {
		case m.override != "":
			from = "lens override"
		case runner != cfg.Runner:
			// ModelFor drops the default's model for a lens on another runtime: that name belongs
			// to the default runner and would be a bad model on this one.
			from = runner + "'s own default"
		case m.phase == distill.PhaseReview && cfg.DistillModel == "" && cfg.TriageModel != "":
			from = "falls back to default mine_model"
		}
		out = append(out, setting{m.key, distill.ModelFor(cfg, l, m.phase), from})
	}
	return out
}

// settingValue renders a setting's value for a person: "runner default" (dim) when it is empty.
func settingValue(s setting) string {
	if s.value == "" {
		return dim("runner default")
	}
	return s.value
}

func newConfigCmd() *cobra.Command {
	c := &cobra.Command{
		Use:     "config",
		GroupID: groupConfig,
		Short:   "Get or set config (runner, models) — default scope or per-lens.",
		RunE:    groupRunE, // a typo'd subcommand is an error, not its help page and exit 0
		Long: `Get or set config knobs that control distillation: runner, mine_model (mining / triage), review_model (review).

The config tree has TWO scopes:
  • DEFAULT scope (no --lens): the baseline runner + models every lens rides unless it overrides.
  • PER-LENS scope (--lens <name>): a lens's runner/model overrides; empty = inherit from default.

Canonical keys:
  runner        distillation runtime: "claude" or "opencode" (also set by ` + "`witness wire <editor>`" + `)
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
			v = "(runner default)" // inheritance is already resolved, so empty can only mean this
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
		// the other writer of this same key: `witness wire <target>` → bindRunner writes
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
	scope := "default scope"
	if lensName != "" {
		scope = "lens " + lensName
	}
	if value != "" {
		fmt.Println(result(okGlyph(), "set "+canonical+" = "+value, scope))
		return nil
	}
	// A clear says what resolution falls back to. `config unset` used to print "set … = (runner
	// default)", which read as if a value had been chosen when the key was removed.
	then := "runner default"
	switch {
	case lensName != "":
		then = "inherits from default"
	case canonical == "runner":
		then = "unbound; $WITNESS_RUNNER, else the built-in default"
	case canonical == "review_model":
		then = "falls back to mine_model"
	}
	fmt.Println(result(okGlyph(), "cleared "+canonical, scope+" → "+then))
	return nil
}

func cmdConfigList(lensName string) error {
	st, err := store.Open()
	if err != nil {
		return err
	}
	defer st.Close()

	var l *lens.Lens
	scope := "default"
	if lensName != "" {
		if err := configCheckLensExists(st, lensName); err != nil {
			return err
		}
		// LoadRegistered does not resolve inheritance: its model/runner fields are exactly the
		// lens.json overrides, which is what tells an override from an inherited value.
		if l, err = lens.LoadRegistered(lensName, st.LensesDir()); err != nil {
			return fmt.Errorf("load lens %q: %w", lensName, err)
		}
		scope = "lens " + lensName
	}
	fmt.Println(title("config", scope, ""))
	fmt.Println()
	for _, s := range effectiveConfig(st, l) {
		fmt.Println("  " + kvRow(s.key, settingValue(s), s.from))
	}
	fmt.Println()
	fmt.Println("  " + footer("precedence: per-lens › default › environment"))
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

// configReadValue reads the EFFECTIVE value of a config key for the given scope — what the engine
// will use, inheritance and the review→mine fallback resolved. lensName="" means default scope.
func configReadValue(st *store.Store, key, lensName string) (string, error) {
	var l *lens.Lens
	if lensName != "" {
		if err := configCheckLensExists(st, lensName); err != nil {
			return "", err
		}
		var err error
		if l, err = lens.LoadRegistered(lensName, st.LensesDir()); err != nil {
			return "", fmt.Errorf("load lens %q: %w", lensName, err)
		}
	}
	for _, s := range effectiveConfig(st, l) {
		if s.key == key {
			return s.value, nil
		}
	}
	return "", fmt.Errorf("unknown config key %q", key)
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
