package cli

import (
	"github.com/spf13/cobra"

	"github.com/patrick-1984/ai-minion/internal/version"
)

func init() {
	Register(func(root *cobra.Command) {
		root.AddCommand(&cobra.Command{
			Use:   "version",
			Short: "Print version, commit, build date and Go version",
			Args:  cobra.NoArgs,
			RunE: func(cmd *cobra.Command, _ []string) error {
				_, err := cmd.OutOrStdout().Write([]byte(version.Get().String()))
				return err
			},
		})
	})
}
