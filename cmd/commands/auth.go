package commands

import (
	"fmt"

	"github.com/IngTian/witness/internal/platform"
	"github.com/IngTian/witness/internal/store"
	"github.com/spf13/cobra"
)

func newAuthCmd() *cobra.Command {
	c := &cobra.Command{Use: "auth", Short: "Sign in a distillation runner.", GroupID: groupSetup}
	for _, name := range platform.RunnerNames() {
		p, _ := platform.ByName(name)
		auth, ok := p.(platform.RunnerAuthenticator)
		if !ok {
			continue
		}
		c.AddCommand(&cobra.Command{
			Use: name, Short: "Sign in to witness's own runner home using device authorization.", Args: cobra.NoArgs,
			RunE: func(cmd *cobra.Command, _ []string) error {
				st, err := store.Open()
				if err != nil {
					return err
				}
				defer st.Close()
				if err := auth.Login(cmd.Context(), st.LoadConfig().RuntimeRoot); err != nil {
					return err
				}
				fmt.Printf("%s runner login saved. Run witness wire %s to choose it as your runner.\n", name, name)
				return nil
			},
		})
	}
	return c
}
