package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"

	"github.com/patrick-1984/ai-minion/internal/config"
	"github.com/patrick-1984/ai-minion/internal/worker/agent"
)

// hubIdentityLine is the one line the major Worker commands print first
// (owner ask A52): the Hub this Worker belongs to and the words of the key it
// pinned, so that a key that does not match the Hub's top bar is seen at
// once. It reads the state read-only and never fails the command: a problem
// becomes part of the line. The line is agent.ReadHubKey's, the same bytes as
// hub_key.line of "minion worker status --json" (tray W10b).
func hubIdentityLine(ctx context.Context, res *config.Resolver) string {
	core, err := config.ResolveWorkerCore(res)
	if err != nil {
		return ""
	}
	return agent.ReadHubKey(ctx, core.StateDir).Line
}

// withHubLine prints the Hub identity line to the command's error stream
// before running it.
func withHubLine(run func(*cobra.Command, []string) error, lookupEnv func(string) (string, bool)) func(*cobra.Command, []string) error {
	return func(cmd *cobra.Command, args []string) error {
		printHubLine(cmd, cmd.ErrOrStderr(), lookupEnv)
		return run(cmd, args)
	}
}

func printHubLine(cmd *cobra.Command, w io.Writer, lookupEnv func(string) (string, bool)) {
	// As root (sudo worker up --as-user …) the state directory is not yet
	// validated; the line is left out rather than read from an unchecked path.
	if runningAsRoot() {
		return
	}
	if lookupEnv == nil {
		lookupEnv = os.LookupEnv
	}
	res := &config.Resolver{Role: config.ScopeWorker, Flags: changedFlags{cmd: cmd, reg: config.Default}, LookupEnv: lookupEnv}
	if file, err := loadConfigFile(res, loadWorkerConfig); err == nil {
		res.File = file
	} else if !errors.Is(err, os.ErrNotExist) {
		return
	}
	line := hubIdentityLine(cmd.Context(), res)
	if line == "" {
		return
	}
	if cmd.Name() == "join" || cmd.Name() == "pair" || cmd.Flags().Changed("join") {
		line = "Before this command: " + line
	}
	_, _ = fmt.Fprintln(w, line)
}
