package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/patrick-1984/ai-minion/internal/config"
	"github.com/patrick-1984/ai-minion/internal/identity"
	"github.com/patrick-1984/ai-minion/internal/worker/agent"
	"github.com/patrick-1984/ai-minion/internal/worker/store"
)

func init() {
	Register(func(root *cobra.Command) {
		workerCommand(root).AddCommand(newWorkerVerifyHubCmd(defaultWorkerVerifyHubDeps()))
	})
}

// workerVerifyHubDeps are the inputs of "minion worker verify-hub" that tests
// replace.
type workerVerifyHubDeps struct {
	stdout     io.Writer
	lookupEnv  func(string) (string, bool)
	loadConfig func(stateDir string) (config.FileSource, error)
	openStore  func(ctx context.Context, stateDir string) (*store.Store, error)
}

func defaultWorkerVerifyHubDeps() workerVerifyHubDeps {
	return workerVerifyHubDeps{
		stdout:     os.Stdout,
		lookupEnv:  os.LookupEnv,
		loadConfig: loadWorkerConfig,
		openStore:  func(ctx context.Context, dir string) (*store.Store, error) { return store.OpenContext(ctx, dir) },
	}
}

func newWorkerVerifyHubCmd(deps workerVerifyHubDeps) *cobra.Command {
	var forget bool
	var pinned string
	cmd := &cobra.Command{
		Use:   "verify-hub <fingerprint>",
		Short: "Check that this Worker is paired with the Hub you expect",
		Long: `Compare the Hub this Worker is paired with against a fingerprint you read on
the Hub host (minion hub identity --words): six words, of which the first four
letters each are enough, or the 52 characters. If it matches, the Hub is
recorded as checked. If it does not match, the Worker stops talking to that Hub
at once and stays disconnected, also after a restart, until you pair it again,
run this command with a fingerprint that matches, or remove the pairing.

--forget --pinned <52 characters> removes the pairing whose pinned Hub key has
exactly that fingerprint (like ssh-keygen -R): the Worker must then pair or
enroll again. It never trusts the disputed key again.

The command needs no running Worker.`,
		Args: func(cmd *cobra.Command, args []string) error {
			if forget {
				if len(args) != 0 {
					return errors.New("--forget takes no fingerprint argument; give the pinned key with --pinned")
				}
				return nil
			}
			if len(args) != 1 && len(args) != identity.WordCount {
				return errors.New("give the Hub fingerprint: six words or 52 characters")
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			return runWorkerVerifyHub(cmd, deps, args, forget, pinned)
		},
	}
	cmd.Flags().BoolVar(&forget, "forget", false, "remove the pairing whose pinned Hub key --pinned names (the Worker must pair again)")
	cmd.Flags().StringVar(&pinned, "pinned", "", "with --forget: the full fingerprint (52 characters) of the pinned Hub key to remove")
	if s, ok := config.Default.Lookup(config.KeyStateDir); ok && s.HasFlag() {
		cmd.Flags().String(s.FlagName(), "", fmt.Sprintf("%s (env %s)", s.Help, s.EnvName()))
	}
	return cmd
}

func runWorkerVerifyHub(cmd *cobra.Command, deps workerVerifyHubDeps, args []string, forget bool, pinned string) error {
	out := deps.stdout
	if out == nil {
		out = cmd.OutOrStdout()
	}
	var expect identity.FingerprintMatcher
	var pinnedFP identity.Fingerprint
	var err error
	if forget {
		if pinnedFP, err = identity.ParseFingerprint(strings.TrimSpace(pinned)); err != nil {
			return errors.New("--forget needs --pinned with the full fingerprint (52 characters) of the pinned Hub key, as minion worker status shows it")
		}
	} else if expect, err = identity.ParseFingerprintInput(strings.Join(args, " ")); err != nil {
		return fmt.Errorf("that is not a fingerprint: %s", strings.TrimPrefix(err.Error(), "identity: "))
	}

	res := &config.Resolver{Role: config.ScopeWorker, Flags: changedFlags{cmd: cmd, reg: config.Default}, LookupEnv: deps.lookupEnv}
	if deps.loadConfig != nil {
		file, err := loadConfigFile(res, deps.loadConfig)
		if err != nil {
			return err
		}
		res.File = file
	}
	dir, err := res.Resolve(config.KeyStateDir)
	if err != nil {
		return err
	}
	ctx := cmd.Context()
	st, err := deps.openStore(ctx, dir.Value)
	if err != nil {
		return fmt.Errorf("open the Worker state in %s: %w", dir.Value, err)
	}
	defer func() { _ = st.Close() }()
	enr := agent.NewEnrollments(st, dir.Value)

	if forget {
		switch err := agent.ForgetHub(ctx, enr, pinnedFP); {
		case errors.Is(err, agent.ErrNotPaired):
			_, _ = fmt.Fprintln(out, "This Worker is not paired with a Hub.")
			return &exitCodeError{ExitError}
		case errors.Is(err, agent.ErrPinnedDiffers):
			return errors.New("the pinned Hub key is not the one --pinned names; nothing was removed (minion worker status shows the pinned key)")
		case err != nil:
			return err
		}
		_, _ = fmt.Fprintln(out, "Removed the pairing with Hub "+pinnedFP.Words()+". Pair this Worker again: minion worker pair --hub-fingerprint <value from the Hub host>.")
		return nil
	}

	r, err := agent.VerifyHub(ctx, enr, expect)
	if errors.Is(err, agent.ErrNotPaired) {
		_, _ = fmt.Fprintln(out, "This Worker is not paired with a Hub.")
		return &exitCodeError{ExitError}
	}
	if err != nil {
		return err
	}
	switch r.Outcome {
	case agent.VerifyMatch:
		_, _ = fmt.Fprintln(out, "The Hub this Worker is paired with matches the fingerprint you gave.")
		_, _ = fmt.Fprintln(out, "Hub fingerprint: "+r.Paired.Words())
		_, _ = fmt.Fprintln(out, "                 "+string(r.Paired))
		return nil
	case agent.VerifyRotated:
		_, _ = fmt.Fprintln(out, "The fingerprint you gave is the Hub key this Worker was checked with; the Hub has rotated its master key")
		_, _ = fmt.Fprintln(out, "since, and the Worker followed the signed rotation. The Hub key now is:")
		_, _ = fmt.Fprintln(out, "Hub fingerprint: "+r.Paired.Words())
		_, _ = fmt.Fprintln(out, "                 "+string(r.Paired))
		return nil
	}
	agent.MismatchBox(out, r.Paired, expect, "You gave:")
	return &exitCodeError{ExitError}
}
