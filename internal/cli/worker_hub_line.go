package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/patrick-1984/ai-minion/internal/config"
	"github.com/patrick-1984/ai-minion/internal/identity"
	"github.com/patrick-1984/ai-minion/internal/worker/agent"
	"github.com/patrick-1984/ai-minion/internal/worker/store"
)

// hubIdentityLine is the one line the major Worker commands print first
// (owner ask A52): the Hub this Worker belongs to and the words of the key it
// pinned, so that a key that does not match the Hub's top bar is seen at
// once. It reads the state read-only and never fails the command: a problem
// becomes part of the line.
func hubIdentityLine(ctx context.Context, res *config.Resolver) string {
	core, err := config.ResolveWorkerCore(res)
	if err != nil {
		return ""
	}
	if _, err := os.Stat(filepath.Join(core.StateDir, store.DBFile)); err != nil {
		return "Hub key: none (this Worker is not paired)"
	}
	st, err := store.OpenReadOnlyContext(ctx, core.StateDir)
	if err != nil {
		return "Hub key: unknown (the Worker state cannot be read)"
	}
	defer func() { _ = st.Close() }()
	enr, ok, err := agent.NewEnrollments(st, core.StateDir).Load(ctx)
	switch {
	case err != nil:
		return "Hub key: unknown (the pairing cannot be read)"
	case !ok:
		return "Hub key: none (this Worker is not paired)"
	}
	fp, err := identity.FingerprintOf(enr.Pin.Pub)
	if err != nil {
		return "Hub key: unknown (the pinned key cannot be read)"
	}
	// The Worker's own verdict decides a mismatch (quarantine); the line does
	// not compare keys itself.
	line := "Hub key: " + fp.Words() + " (compare with the Hub's top bar)"
	if enr.Check() == store.HubCheckMismatch {
		line = "Hub key: " + fp.Words() + " · MISMATCH: the Hub presented another key; the Worker refuses it (minion worker status)"
	}
	return line
}

// withHubLine prints the Hub identity line to the command's error stream
// before running it.
func withHubLine(run func(*cobra.Command, []string) error, lookupEnv func(string) (string, bool)) func(*cobra.Command, []string) error {
	return func(cmd *cobra.Command, args []string) error {
		// LocalSystem is refused by the command itself; the line reads the
		// config and the Worker database, which a SYSTEM process must not
		// touch first (a state directory on a share would be contacted with
		// the machine account).
		if !systemAccount() {
			printHubLine(cmd, cmd.ErrOrStderr(), lookupEnv)
		}
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
