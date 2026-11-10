package cli

import (
	"context"
	"io"

	"github.com/spf13/cobra"

	"github.com/patrick-1984/ai-minion/internal/proto"
)

// NewRoot builds a fresh "minion" root command with every registered command
// attached. It builds a new tree on each call so that tests do not share
// command state.
func NewRoot() *cobra.Command {
	return newRoot(&defaultRegistry)
}

func newRoot(reg *Registry) *cobra.Command {
	root := &cobra.Command{
		Use:           "minion",
		Short:         "Minion Hub and Minion Worker for terminal and agent sessions",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	reg.Apply(root)
	return root
}

// Execute runs the minion command line with args (without the program name)
// and returns the process exit code: 0 on success, 1 on any error. Errors are
// written to stderr.
func Execute(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	return executeRoot(ctx, NewRoot(), args, stdout, stderr)
}

// maxErrorText bounds the error line written to stderr, in runes.
const maxErrorText = 8192

// executeRoot runs root and writes a failure as one "Error: " line. This is the
// terminal boundary for errors: an error can carry text that came from another
// process (a remote diagnostic, a message in a response), so what is written is
// made safe here whatever the error wraps. Control sequences, bidi controls and
// line breaks cannot reach the operator's terminal, so an error cannot clear the
// screen, rewrite the clipboard or pose as a second message; the length is
// bounded and the text is valid UTF-8.
func executeRoot(ctx context.Context, root *cobra.Command, args []string, stdout, stderr io.Writer) int {
	root.SetArgs(args)
	root.SetOut(stdout)
	root.SetErr(stderr)
	if err := root.ExecuteContext(ctx); err != nil {
		_, _ = io.WriteString(stderr, "Error: "+proto.SafeText(err.Error(), maxErrorText)+"\n")
		return 1
	}
	return 0
}
