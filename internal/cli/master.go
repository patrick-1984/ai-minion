package cli

import (
	"github.com/spf13/cobra"
)

func init() {
	Register(func(root *cobra.Command) {
		master := &cobra.Command{
			Use: "master", Short: "Prepare one terminal for a Minion master nomination",
		}
		master.AddCommand(&cobra.Command{
			Use: "claude [-- CLAUDE_ARGS...]", Short: "Launch Claude Code with a session-only Minion attachment",
			Long: "From a shared Linux tmux pane, use exec minion master claude. " +
				"This deliberately starts Claude in that pane. Nominate the prepared session in Access > Minion control.",
			DisableFlagParsing: true,
			RunE:               runMasterClaude,
		})
		master.AddCommand(&cobra.Command{
			Use: "mcp SOCKET", Hidden: true, Args: cobra.ExactArgs(1), RunE: runMasterMCP,
		})
		root.AddCommand(master)
	})
}
