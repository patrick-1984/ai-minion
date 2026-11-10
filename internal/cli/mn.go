package cli

import (
	"context"
	"errors"
	"io"
	"strings"

	"github.com/spf13/cobra"
)

// IsMN reports whether the program was started under the name `mn` (or
// `mn.exe`), through a symlink or a hardlink to the minion binary. Both path
// separators are understood so that a Windows argv[0] works everywhere.
func IsMN(arg0 string) bool {
	base := arg0
	if i := strings.LastIndexAny(base, `/\`); i >= 0 {
		base = base[i+1:]
	}
	base = strings.ToLower(base)
	return strings.TrimSuffix(base, ".exe") == "mn"
}

// Run is the entry point of the binary: argv is os.Args including the program
// name. Under the name `mn` the command tree is the same as for `minion`, with
// the op subcommands at the top level, and only the name in the usage texts
// changes. It returns the process exit status: 0 on success, the status of a
// write outcome (3 failed_not_sent, 4 partial or unknown), 2 for a usage error
// and 1 for every other error. Errors are written to stderr.
func Run(ctx context.Context, argv []string, stdout, stderr io.Writer) int {
	root := NewRoot()
	if len(argv) > 0 && IsMN(argv[0]) {
		root.Use = "mn"
	}
	var args []string
	if len(argv) > 1 {
		args = argv[1:]
	}
	return execute(ctx, root, args, stdout, stderr)
}

// execute runs root with args and maps the error to the exit status.
func execute(ctx context.Context, root *cobra.Command, args []string, stdout, stderr io.Writer) int {
	root.SetArgs(args)
	root.SetOut(stdout)
	root.SetErr(stderr)
	err := root.ExecuteContext(ctx)
	if err == nil {
		return ExitOK
	}
	var ec *exitCodeError
	if errors.As(err, &ec) {
		return ec.code
	}
	_, _ = io.WriteString(stderr, "Error: "+err.Error()+"\n")
	var ue *usageError
	if errors.As(err, &ue) {
		return ExitUsage
	}
	return ExitError
}
