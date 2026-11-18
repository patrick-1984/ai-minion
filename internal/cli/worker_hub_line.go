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
	hub, err := config.ResolveWorkerHub(res)
	if err != nil {
		return ""
	}
	server := hub.Server
	if server == "" {
		server = "no Hub set"
	}
	expected := ""
	var want identity.FingerprintMatcher
	if hub.Fingerprint != "" {
		if m, err := identity.ParseFingerprintInput(hub.Fingerprint); err == nil {
			want, expected = m, m.Words()
		}
	}
	if _, err := os.Stat(filepath.Join(core.StateDir, store.DBFile)); err != nil {
		if expected != "" {
			return fmt.Sprintf("Hub: %s · not paired yet · expected key: %s", server, expected)
		}
		return fmt.Sprintf("Hub: %s · not paired yet", server)
	}
	st, err := store.OpenReadOnlyContext(ctx, core.StateDir)
	if err != nil {
		return fmt.Sprintf("Hub: %s · the Worker state cannot be read: %v", server, err)
	}
	defer func() { _ = st.Close() }()
	enr, ok, err := agent.NewEnrollments(st, core.StateDir).Load(ctx)
	switch {
	case err != nil:
		return fmt.Sprintf("Hub: %s · the pairing cannot be read: %v", server, err)
	case !ok:
		if expected != "" {
			return fmt.Sprintf("Hub: %s · not paired yet · expected key: %s", server, expected)
		}
		return fmt.Sprintf("Hub: %s · not paired yet", server)
	}
	fp, err := identity.FingerprintOf(enr.Pin.Pub)
	if err != nil {
		return fmt.Sprintf("Hub: %s · the pinned key cannot be read", server)
	}
	line := fmt.Sprintf("Hub: %s · key %s", server, fp.Words())
	if enr.Check() == store.HubCheckMismatch {
		line += " · MISMATCH: this Hub's key is not the one it was checked against; the Worker refuses it (minion worker status)"
	} else if !want.IsZero() && !want.Matches(enr.Pin.Pub) {
		line += " · MISMATCH: hub_fingerprint is " + expected + ", not the paired Hub's key (minion worker status)"
	}
	return line
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
	if lookupEnv == nil {
		lookupEnv = os.LookupEnv
	}
	res := &config.Resolver{Role: config.ScopeWorker, Flags: changedFlags{cmd: cmd, reg: config.Default}, LookupEnv: lookupEnv}
	if file, err := loadConfigFile(res, loadWorkerConfig); err == nil {
		res.File = file
	} else if !errors.Is(err, os.ErrNotExist) {
		return
	}
	if line := hubIdentityLine(cmd.Context(), res); line != "" {
		_, _ = fmt.Fprintln(w, line)
	}
}
