package cli

import (
	"os"

	"github.com/spf13/cobra"

	"github.com/patrick-1984/ai-minion/internal/filexfer"
)

// minion worker files-probe (files.v1, BRD 5.3): the child the Worker runs
// once at start, in a scratch folder the Worker prepared, to learn whether
// openat2 and statx mount ids work in this process environment. A seccomp
// filter that traps one of them ends this child, not the Worker. It is not a
// user command.
func init() {
	Register(func(root *cobra.Command) {
		workerCommand(root).AddCommand(&cobra.Command{
			Use:    "files-probe",
			Short:  "Check how this system confines file transfers (run by the Worker itself)",
			Hidden: true,
			Args:   cobra.NoArgs,
			RunE: func(cmd *cobra.Command, _ []string) error {
				if code := filexfer.ProbeChild(cmd.OutOrStdout()); code != 0 {
					os.Exit(code)
				}
				return nil
			},
		})
	})
}
