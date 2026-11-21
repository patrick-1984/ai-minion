//go:build !linux

package cli

import (
	"errors"

	"github.com/spf13/cobra"
)

func init() {
	Register(func(root *cobra.Command) {
		root.AddCommand(&cobra.Command{
			Use:   "hub-updater",
			Short: "Update the Hub on a TrueNAS host (Linux only)",
			Args:  cobra.NoArgs,
			RunE: func(*cobra.Command, []string) error {
				return errors.New("hub-updater runs on the TrueNAS host (Linux) only")
			},
		})
	})
}
