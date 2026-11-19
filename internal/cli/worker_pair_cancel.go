package cli

// "minion worker pair cancel --attempt A" (tray tickets v3, C9 and W8b):
// cancels the pairing that the running Worker does by itself, for the tray's
// "Cancel pairing". It is its own action, separate from leaving the Hub.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/patrick-1984/ai-minion/internal/client"
	"github.com/patrick-1984/ai-minion/internal/config"
	"github.com/patrick-1984/ai-minion/internal/worker/agent"
	"github.com/patrick-1984/ai-minion/internal/worker/localapi"
)

// workerPairCancelDeps are the inputs of "minion worker pair cancel" that
// tests replace.
type workerPairCancelDeps struct {
	lookupEnv  func(string) (string, bool)
	loadConfig func(stateDir string) (config.FileSource, error)
	// call is the local control socket of this user.
	call func(ctx context.Context, op string, args any) (json.RawMessage, error)
}

func defaultWorkerPairCancelDeps() workerPairCancelDeps {
	return workerPairCancelDeps{
		lookupEnv:  os.LookupEnv,
		loadConfig: loadWorkerConfig,
		call: func(ctx context.Context, op string, args any) (json.RawMessage, error) {
			return client.Local().Call(ctx, op, args)
		},
	}
}

// pairCancelResult is the JSON result of "worker pair cancel" (besides ok,
// code and message).
type pairCancelResult struct {
	StateDir      string   `json:"state_dir,omitempty"`
	Attempt       string   `json:"attempt,omitempty"`
	Cancelled     bool     `json:"cancelled"`
	ResumeDeleted bool     `json:"resume_deleted,omitempty"`
	Cleared       []string `json:"cleared,omitempty"`
	Kept          []string `json:"kept,omitempty"`
}

func newWorkerPairCancelCmd(deps workerPairCancelDeps) *cobra.Command {
	var asJSON bool
	var attempt string
	cmd := &cobra.Command{
		Use:   "cancel --attempt A",
		Short: "Cancel the pairing that the running Worker does in the background",
		Long: `Cancel the pairing that the running Worker does by itself (started by
"minion worker join <url> --background", for example from the tray). Give the
attempt of the code shown, as "minion worker status --json" reports it
(pairing.attempt): a code that was replaced since is refused with exit status
27 and nothing is changed.

The Worker stops pairing, forgets the code and its resume record, and does not
pair by itself with this Hub address again, also not after a restart, until a
new "minion worker join <url> --background". The Worker key and the sharing
choice are kept. The Hub address and the expected Hub fingerprint are removed
from the config file only when the background join saved them and they are
unchanged since; values that were there before, or were edited, stay.

A Worker that was paired meanwhile is refused with exit status 27 and its
pairing stays: leaving the Hub is a separate action. Without a running Worker
the exit status is 24.

--json prints one JSON object {"ok","code","message","cancelled",...}.`,
		Args: noArgsRedacted,
		RunE: func(cmd *cobra.Command, _ []string) error {
			res, err := runWorkerPairCancel(cmd, deps, attempt)
			if err == nil {
				human := cmd.OutOrStdout()
				if asJSON {
					human = cmd.ErrOrStderr()
				}
				_, _ = fmt.Fprintln(human, "Pairing cancelled: the Worker shows no code and does not pair by itself again until minion worker join <url> --background.")
				if len(res.Cleared) > 0 {
					_, _ = fmt.Fprintf(human, "Removed from the config file (saved by the background join): %s.\n", strings.Join(res.Cleared, ", "))
				}
				if len(res.Kept) > 0 {
					_, _ = fmt.Fprintf(human, "Kept in the config file (not saved by the background join, or changed since): %s.\n", strings.Join(res.Kept, ", "))
				}
			}
			return emitResult(cmd.OutOrStdout(), cmd.ErrOrStderr(), asJSON, res, err)
		},
	}
	cmd.Flags().StringVar(&attempt, "attempt", "", "the pairing attempt to cancel (pairing.attempt of minion worker status --json)")
	cmd.Flags().BoolVar(&asJSON, "json", false, "print one JSON result object")
	addNamedSettingFlags(cmd, config.Default, config.KeyStateDir)
	return cmd
}

func runWorkerPairCancel(cmd *cobra.Command, deps workerPairCancelDeps, attempt string) (pairCancelResult, error) {
	res := pairCancelResult{Attempt: attempt}
	if strings.TrimSpace(attempt) == "" {
		return res, usagef("give the pairing attempt: minion worker pair cancel --attempt A (pairing.attempt of minion worker status --json)")
	}
	r := &config.Resolver{Role: config.ScopeWorker, Flags: changedFlags{cmd: cmd, reg: config.Default}, LookupEnv: deps.lookupEnv}
	if deps.loadConfig != nil {
		file, err := loadConfigFile(r, deps.loadConfig)
		if err != nil {
			return res, err
		}
		r.File = file
	}
	dir, err := r.Resolve(config.KeyStateDir)
	if err != nil {
		return res, err
	}
	stateDir, err := filepath.Abs(dir.Value)
	if err != nil {
		return res, err
	}
	res.StateDir = stateDir
	ctx := cmdContext(cmd)
	before, err := liveReply(ctx, deps.call)
	switch {
	case errors.Is(err, client.ErrWorkerNotRunning):
		return res, newResultError(ExitNotRunning, "no Worker is running for this user, so no pairing runs; nothing was changed")
	case err != nil:
		return res, fmt.Errorf("cannot ask the running Worker: %w", err)
	case before.StateDir == "" || !sameDir(before.StateDir, stateDir):
		return res, newResultError(ExitNotRunning, fmt.Sprintf("no Worker is running for the state directory %s (the running one uses %s); nothing was changed", stateDir, before.StateDir))
	}
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	raw, err := deps.call(cctx, localapi.QualifiedOp(agent.OpLocalPairCancel, []string{agent.FeaturePairCancel}),
		agent.LocalPairCancelArgs{Attempt: attempt, Instance: before.Instance})
	if err != nil {
		if rerr := lifecycleRefusal(err); rerr != nil {
			return res, rerr
		}
		return res, err
	}
	var rep agent.PairCancelResult
	if err := json.Unmarshal(raw, &rep); err != nil {
		return res, fmt.Errorf("the running Worker returned an invalid result: %w", err)
	}
	res.Cancelled, res.ResumeDeleted, res.Cleared, res.Kept = rep.Cancelled, rep.ResumeDeleted, rep.Cleared, rep.Kept
	return res, nil
}
