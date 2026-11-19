package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/patrick-1984/ai-minion/internal/trayactions"
)

func init() {
	Register(func(root *cobra.Command) {
		root.AddCommand(newTrayActionsCmd())
	})
}

// trayActionsResult is the JSON result of "minion tray-actions": the action
// table compiled into this minion, so that a tray can tell which controls
// this minion supports.
type trayActionsResult struct {
	Actions      []trayactions.Action             `json:"actions"`
	Required     []string                         `json:"required"`
	Elevated     map[trayactions.Elevate][]string `json:"elevated"`
	Retry        map[string][]string              `json:"session_loss_retry"`
	Placeholders map[string]string                `json:"placeholders"`
}

func trayActions() trayActionsResult {
	return trayActionsResult{
		Actions:  trayactions.All,
		Required: trayactions.Required,
		Elevated: map[trayactions.Elevate][]string{
			trayactions.ElevateE1: trayactions.ElevatedArgv(trayactions.ElevateE1),
			trayactions.ElevateE2: trayactions.ElevatedArgv(trayactions.ElevateE2),
		},
		Retry: map[string][]string{
			"sessions_would_end": trayactions.AcceptLossArgv,
			"count_unavailable":  trayactions.AcceptUnknownLossArgv,
		},
		Placeholders: trayactions.Placeholders,
	}
}

func newTrayActionsCmd() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:    "tray-actions",
		Short:  "Print the tray's action table: every tray control and its minion command",
		Hidden: true,
		Long: `Print the action table of the tray app compiled into this minion: for every
control of the tray menu, the program and argv that do the same thing, the
standard input, the confirmation, the elevated retry (E1/E2), the effect and
how the effect is observed. Without --json it prints the parity table of
docs/guides/tray.md. --json prints one JSON object {"ok","code","message",
"actions","required","elevated","session_loss_retry","placeholders"}.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if !asJSON {
				_, err := fmt.Fprint(cmd.OutOrStdout(), trayactions.ParityMarkdown())
				return err
			}
			return emitResult(cmd.OutOrStdout(), cmd.ErrOrStderr(), true, trayActions(), nil)
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "print one JSON result object")
	return cmd
}
