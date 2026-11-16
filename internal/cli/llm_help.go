package cli

import (
	"io"

	"github.com/spf13/cobra"

	"github.com/patrick-1984/ai-minion/internal/llmguide"
)

func init() {
	Register(func(root *cobra.Command) {
		root.AddCommand(&cobra.Command{
			Use:   "llm-help",
			Short: "Print the operating guide for AI agents (LLMs): detect, install, pair, use sessions, become a Minion master",
			Args:  cobra.NoArgs,
			RunE: func(cmd *cobra.Command, _ []string) error {
				_, err := io.WriteString(cmd.OutOrStdout(), llmguide.Text)
				return err
			},
		})
	})
}
