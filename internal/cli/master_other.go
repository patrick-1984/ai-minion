//go:build !linux || android

package cli

import (
	"errors"

	"github.com/spf13/cobra"
)

func runMasterClaude(*cobra.Command, []string) error { return unsupportedMaster() }
func runMasterMCP(*cobra.Command, []string) error    { return unsupportedMaster() }

func unsupportedMaster() error {
	return errors.New("the Minion master attachment requires Claude Code 2.1.330 on Linux with tmux")
}
