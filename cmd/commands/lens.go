package commands

import (
	"fmt"
	"log/slog"
	"slices"

	"github.com/IngTian/claude-witness/internal/lens"
	"github.com/IngTian/claude-witness/internal/store"
	"github.com/spf13/cobra"
)

func newLensCmd() *cobra.Command {
	lensCmd := &cobra.Command{
		Use:   "lens",
		Short: "Manage global observation lenses.",
		Long:  "Manage the central lens registry. Registered and enabled lenses run globally across every session alongside the always-on default lens.",
	}
	lensCmd.AddCommand(
		&cobra.Command{
			Use:   "register <name> <file>",
			Short: "Register or replace a lens definition.",
			Long:  "Copy a lens markdown definition into the witness store. Later edits to the source file do not affect the registered snapshot until you register it again.",
			Args:  cobra.ExactArgs(2),
			RunE:  func(_ *cobra.Command, args []string) error { return cmdLens(append([]string{"register"}, args...)) },
		},
		&cobra.Command{
			Use:   "deregister <name>",
			Short: "Remove a registered lens definition.",
			Args:  cobra.ExactArgs(1),
			RunE:  func(_ *cobra.Command, args []string) error { return cmdLens(append([]string{"deregister"}, args...)) },
		},
		&cobra.Command{
			Use:   "enable <name>",
			Short: "Enable a registered lens for every session.",
			Args:  cobra.ExactArgs(1),
			RunE:  func(_ *cobra.Command, args []string) error { return cmdLens(append([]string{"enable"}, args...)) },
		},
		&cobra.Command{
			Use:   "disable <name>",
			Short: "Stop running a lens on new distillation work.",
			Args:  cobra.ExactArgs(1),
			RunE:  func(_ *cobra.Command, args []string) error { return cmdLens(append([]string{"disable"}, args...)) },
		},
		&cobra.Command{
			Use:   "list",
			Short: "List registered lenses and enabled state.",
			Args:  cobra.NoArgs,
			RunE:  func(_ *cobra.Command, _ []string) error { return cmdLens([]string{"list"}) },
		},
	)
	return lensCmd
}

// cmdLens manages the central, global lens registry. Lenses are defined once and
// shared across every session (alongside the always-on "default" lens):
//
//	witness lens register <name> <file>   add/replace a lens definition
//	witness lens deregister <name>        remove a lens definition
//	witness lens enable <name>            run this lens on every session
//	witness lens disable <name>           stop running it
//	witness lens list                     show registered lenses + enabled state
func cmdLens(args []string) error {
	st, err := store.Open()
	if err != nil {
		return err
	}
	defer st.Close()
	if len(args) == 0 {
		return fmt.Errorf("usage: witness lens <register <name> <file>|deregister <name>|enable <name>|disable <name>|list>")
	}
	switch args[0] {
	case "register":
		if len(args) < 3 {
			return fmt.Errorf("usage: witness lens register <name> <file>")
		}
		if err := st.RegisterLens(args[1], args[2]); err != nil {
			return err
		}
		fmt.Printf("registered lens %q\n", args[1])
	case "deregister":
		if len(args) < 2 {
			return fmt.Errorf("usage: witness lens deregister <name>")
		}
		if err := st.DeregisterLens(args[1]); err != nil {
			return err
		}
		fmt.Printf("deregistered lens %q\n", args[1])
	case "enable":
		if len(args) < 2 || args[1] == "" {
			return fmt.Errorf("usage: witness lens enable <name>")
		}
		if !slices.Contains(st.RegisteredLenses(), args[1]) {
			return fmt.Errorf("lens %q is not registered (run: witness lens register %s <file>)", args[1], args[1])
		}
		if err := st.EnableLens(args[1]); err != nil {
			return err
		}
		fmt.Printf("enabled lens %q (runs on every session)\n", args[1])
	case "disable":
		if len(args) < 2 || args[1] == "" {
			return fmt.Errorf("usage: witness lens disable <name>")
		}
		if err := st.DisableLens(args[1]); err != nil {
			return err
		}
		fmt.Printf("disabled lens %q\n", args[1])
	case "list":
		enabled := st.LoadConfig().EnabledLenses
		reg := st.RegisteredLenses()
		// The default lens always runs and isn't in the registry; show it first so
		// `lens list` reflects what actually runs, not just the registered extras.
		fmt.Printf("  %s %s  %s\n", green("✓"), "default", dim("(built-in, always on)"))
		if len(reg) == 0 {
			fmt.Println(dim("  no additional lenses registered"))
			return nil
		}
		for _, name := range reg {
			if slices.Contains(enabled, name) {
				fmt.Printf("  %s %s  %s\n", green("✓"), name, dim("(enabled — runs on every session)"))
			} else {
				fmt.Printf("  %s %s  %s\n", dim("·"), name, dim("(registered, disabled)"))
			}
		}
	default:
		return fmt.Errorf("unknown lens subcommand %q (want register|deregister|enable|disable|list)", args[0])
	}
	return nil
}

// activeLenses returns the default lens (always on) + every enabled, registered
// lens. All are global — they run on every session.
func activeLenses(st *store.Store) ([]*lens.Lens, error) {
	out := []*lens.Lens{}
	if p, err := lens.LoadDefault(); err == nil {
		out = append(out, p)
	} else {
		return nil, fmt.Errorf("load default lens: %w", err)
	}
	for _, name := range st.LoadConfig().EnabledLenses {
		l, err := lens.LoadRegistered(name, st.LensesDir())
		if err != nil {
			slog.Warn("enabled lens not loadable; skipping", "lens", name, "err", err)
			continue
		}
		out = append(out, l)
	}
	return out, nil
}
