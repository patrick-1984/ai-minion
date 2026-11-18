package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/patrick-1984/ai-minion/internal/client"
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
	// lockState takes the lifecycle lock of the state directory without
	// waiting (held by every running Worker); an error means a Worker runs.
	lockState func(stateDir string) (io.Closer, error)
	// call is the local control socket of this user.
	call func(ctx context.Context, op string) (json.RawMessage, error)
	// sleep waits between the polls of the running Worker's status.
	sleep func(time.Duration)
}

func defaultWorkerVerifyHubDeps() workerVerifyHubDeps {
	return workerVerifyHubDeps{
		stdout:     os.Stdout,
		lookupEnv:  os.LookupEnv,
		loadConfig: loadWorkerConfig,
		openStore:  func(ctx context.Context, dir string) (*store.Store, error) { return store.OpenContext(ctx, dir) },
		lockState:  agent.LockLifecycle,
		call: func(ctx context.Context, op string) (json.RawMessage, error) {
			return client.Local().Call(ctx, op, nil)
		},
		sleep: time.Sleep,
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

The command needs no running Worker. A running Worker of v0.11.0 or later is
told through the shared state and the command reports when it confirmed that it
stopped connecting. A running older Worker cannot apply the check: the command
then changes nothing and asks you to stop it first (minion worker down).`,
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
	if ctx == nil {
		ctx = context.Background()
	}
	// Before the state is opened (opening migrates it): a running Worker must
	// be one that applies the stored check, else nothing is changed.
	running, err := runningWorkerOf(ctx, deps, dir.Value)
	if err != nil {
		return err
	}
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
		_, _ = fmt.Fprintln(out, "Removed the pairing with Hub "+pinnedFP.Words()+".")
		if removed, err := deleteWorkerSetting(dir.Value, config.KeyInviteKey); err != nil {
			_, _ = fmt.Fprintf(out, "Could not remove invite_key from the config file (%v); remove it by hand.\n", err)
		} else if removed {
			_, _ = fmt.Fprintln(out, "Removed the invitation key (invite_key) from the config file.")
		}
		_, _ = fmt.Fprintln(out, "This Worker does not enroll again by itself from an invitation key that is still configured "+
			"(--invite-key, MINION_INVITE_KEY_FILE); remove it from the service configuration.")
		_, _ = fmt.Fprintln(out, "Pair this Worker again: minion worker pair --hub-fingerprint <value from the Hub host>.")
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
		_, _ = fmt.Fprintln(out, "The fingerprint you gave is a Hub key this Worker was checked with; the Hub has rotated its master key")
		_, _ = fmt.Fprintln(out, "since, and the Worker followed the signed rotation. The Hub key now is:")
		_, _ = fmt.Fprintln(out, "Hub fingerprint: "+r.Paired.Words())
		_, _ = fmt.Fprintln(out, "                 "+string(r.Paired))
		if r.Quarantined {
			agent.QuarantineNote(out, r.Paired)
			return &exitCodeError{ExitError}
		}
		return nil
	case agent.VerifyNeedFull:
		_, _ = fmt.Fprintln(out, "The words match the paired Hub, but this Worker is quarantined: a fingerprint given earlier did not match.")
		_, _ = fmt.Fprintln(out, "Only the full fingerprint (52 characters) read on the Hub host ends the quarantine:")
		_, _ = fmt.Fprintln(out, "    minion worker verify-hub <52 characters>")
		return &exitCodeError{ExitError}
	}
	agent.MismatchBox(out, r.Paired, expect, "You gave:")
	reportQuarantineApplied(ctx, out, deps, dir.Value, running)
	return &exitCodeError{ExitError}
}

// runningWorkerOf reports whether a Worker runs on the state directory dir
// and, when one does, that it is one that applies a stored hub_check (v0.11.0
// or later). A running Worker that cannot be asked, or an older one, is an
// error that says how to stop it: the command then changes nothing (an older
// Worker would reconnect and could not open the migrated state again).
func runningWorkerOf(ctx context.Context, deps workerVerifyHubDeps, dir string) (bool, error) {
	if deps.lockState == nil {
		return false, nil
	}
	l, err := deps.lockState(dir)
	if err == nil {
		_ = l.Close()
		return false, nil
	}
	if !errors.Is(err, agent.ErrStateInUse) {
		return false, err
	}
	reply, ok := liveStatusOf(ctx, deps, dir)
	switch {
	case !ok:
		return false, fmt.Errorf("a Worker is running on the state directory %s and does not answer on this user's control socket; "+
			"nothing was changed. Stop it first (minion worker down, as the user that runs it), then run this command again", dir)
	case !reply.HubCheckAware:
		return false, errors.New("the running Worker is older than v0.11.0: it cannot apply this check and would keep or resume its " +
			"connection to the Hub; nothing was changed. Stop it first with: minion worker down - then run this command again " +
			"(and upgrade the Worker before you start it again)")
	}
	return true, nil
}

// liveStatusOf asks the running Worker of this user for its live status; ok
// is false when none answers for the state directory dir.
func liveStatusOf(ctx context.Context, deps workerVerifyHubDeps, dir string) (agent.LiveStatusReply, bool) {
	var reply agent.LiveStatusReply
	if deps.call == nil {
		return reply, false
	}
	cctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	raw, err := deps.call(cctx, agent.OpLocalStatus)
	if err != nil || json.Unmarshal(raw, &reply) != nil || reply.StateDir == "" || !sameDir(reply.StateDir, dir) {
		return agent.LiveStatusReply{}, false
	}
	return reply, true
}

// reportQuarantineApplied says what happened to the running Worker after a
// mismatch was stored: "stopped talking" only once its status confirms it.
func reportQuarantineApplied(ctx context.Context, out io.Writer, deps workerVerifyHubDeps, dir string, running bool) {
	if !running {
		_, _ = fmt.Fprintln(out, "No Worker is running on this state directory; it will not connect to that Hub when it starts.")
		return
	}
	for i := 0; i < 40; i++ {
		if reply, ok := liveStatusOf(ctx, deps, dir); ok && reply.State == string(agent.ConnIdle) && reply.Reason == agent.ReasonQuarantined {
			_, _ = fmt.Fprintln(out, "The running Worker confirmed: it stopped talking to that Hub and stays disconnected.")
			return
		}
		if deps.sleep != nil {
			deps.sleep(250 * time.Millisecond)
		}
	}
	_, _ = fmt.Fprintln(out, "The running Worker has not confirmed yet that it stopped talking to that Hub; check it with: minion worker status")
}
