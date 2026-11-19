package cli

// "minion worker leave" (tray tickets v3, W9; architecture v2 section 4.4
// "Leave this Hub"): one command, bound to one state directory, that stops
// the Worker of that directory and returns it to the unconfigured state. It
// replaces "worker down && worker reset" for the tray. The Hub is not told:
// the Worker stays listed there until a user removes it.

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/patrick-1984/ai-minion/internal/config"
	"github.com/patrick-1984/ai-minion/internal/identity"
	"github.com/patrick-1984/ai-minion/internal/worker/agent"
	"github.com/patrick-1984/ai-minion/internal/worker/store"
)

func init() {
	Register(func(root *cobra.Command) {
		workerCommand(root).AddCommand(newWorkerLeaveCmd(defaultWorkerLeaveDeps()))
	})
}

// Steps of "worker leave" before the reset (the reset adds agent.ResetStep*).
const (
	leaveStepVerified = "verified"
	leaveStepStopped  = "stopped"
)

// leaveStillListed is said by every successful leave, in the text and the
// JSON result.
const leaveStillListed = "the Worker stays listed on the Hub until you remove it there"

// workerLeaveDeps are the inputs of "minion worker leave" that tests replace.
type workerLeaveDeps struct {
	stdin      io.Reader
	lookupEnv  func(string) (string, bool)
	loadConfig func(stateDir string) (config.FileSource, error)
	// down stops the running Worker (with the session-loss preflight of
	// "worker down"); its call also asks the running Worker for its status.
	down workerDownDeps
	// lockState takes the lifecycle lock of a state directory without
	// waiting (agent.LockLifecycle).
	lockState func(stateDir string) (io.Closer, error)
	// lockEnroll takes the enrollment lock (agent.LockEnrollment).
	lockEnroll func(ctx context.Context, stateDir string) (func(), error)
	// openStore opens the Worker state of the directory (NoMigrate).
	openStore func(ctx context.Context, stateDir string) (agent.ResetStore, io.Closer, error)
	// sleep and stopWait pace the wait for the stopped Worker to release
	// its lifecycle lock.
	sleep    func(time.Duration)
	stopWait time.Duration
}

func defaultWorkerLeaveDeps() workerLeaveDeps {
	return workerLeaveDeps{
		stdin:      os.Stdin,
		lookupEnv:  os.LookupEnv,
		loadConfig: loadWorkerConfig,
		down:       defaultWorkerDownDeps(),
		lockState:  agent.LockLifecycle,
		lockEnroll: agent.LockEnrollment,
		openStore: func(ctx context.Context, dir string) (agent.ResetStore, io.Closer, error) {
			st, err := store.OpenContext(ctx, dir, store.NoMigrate())
			if err != nil {
				return nil, nil, err
			}
			return st, st, nil
		},
		sleep:    time.Sleep,
		stopWait: 30 * time.Second,
	}
}

// leaveOptions are the flags of "worker leave".
type leaveOptions struct {
	yes           bool
	expectFP      string
	expectEpoch   uint64
	acceptLoss    int
	acceptUnknown bool
	json          bool
}

// leaveLeft is a setting that leave could not remove because it does not
// come from the config file.
type leaveLeft struct {
	Key    string `json:"key"`
	Source string `json:"source"`
	Name   string `json:"name"`
}

// leaveResult is the JSON result of "worker leave" (besides ok, code and
// message).
type leaveResult struct {
	StateDir     string `json:"state_dir"`
	Left         bool   `json:"left"`
	Stopped      bool   `json:"stopped"`
	SessionsLost *int   `json:"sessions_lost,omitempty"`
	// StepsDone lists the steps that finished, also after a failure.
	StepsDone []string `json:"steps_done"`
	// Removed are the config keys removed; Kept the ones kept (C11); NotRemoved
	// the settings from the environment that stay in effect.
	Removed      []string    `json:"removed,omitempty"`
	Kept         []string    `json:"kept,omitempty"`
	NotRemoved   []leaveLeft `json:"not_removed,omitempty"`
	PauseCleared bool        `json:"pause_cleared,omitempty"`
	// StillListedOnHub is always true: the Hub is not told.
	StillListedOnHub bool   `json:"still_listed_on_hub"`
	Note             string `json:"note"`
}

func newWorkerLeaveCmd(deps workerLeaveDeps) *cobra.Command {
	var o leaveOptions
	cmd := &cobra.Command{
		Use:   "leave",
		Short: "Stop this Worker and leave its Hub (the Worker stays listed on the Hub)",
		Long: `Leave the Hub: stop the Worker of one state directory and return it to the
unconfigured state, so that it can join another Hub.

It stops the Worker of that state directory first (sessions that live inside
the Worker, Windows ConPTY, end with it: exit status 23 with sessions_lost N,
repeat with --accept-loss N; 29 when they cannot be counted, repeat with
--accept-unknown-loss). Then it deletes the Worker key pair, the pinned Hub
key and the enrollment, the share selection, the local policy and the Hub
address, and clears a pause. hub_fingerprint is removed only when a join
saved it and it is unchanged since; a value provisioned before the join,
edited after it, or saved by an older join is kept and reported ("kept").
allow_new_sessions and the other machine settings stay. Settings that come
from environment variables or service flags cannot be removed here; the
environment ones are reported ("not_removed"). Your tmux sessions and the
session counter are not touched.

The Hub is not told: the Worker stays listed on the Hub until you remove it
there.

With --yes, give the Worker and the enrollment you mean, as "minion worker
status --json" reports them: --expect-worker-fingerprint (worker_fingerprint,
"" for none) and --expect-enrollment-epoch (enrollment_epoch). When either has
changed, the exit status is 27 and nothing is deleted. Without --yes the
command shows what it deletes and asks.

--json prints one JSON object; after a failure, steps_done lists what was done.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			res, err := runWorkerLeave(cmd, deps, o)
			if err == nil {
				human := cmd.OutOrStdout()
				if o.json {
					human = cmd.ErrOrStderr()
				}
				printLeaveReport(human, res)
			}
			return emitResult(cmd.OutOrStdout(), cmd.ErrOrStderr(), o.json, res, err)
		},
	}
	cmd.Flags().BoolVarP(&o.yes, "yes", "y", false, "do not ask; needs --expect-worker-fingerprint and --expect-enrollment-epoch")
	cmd.Flags().StringVar(&o.expectFP, "expect-worker-fingerprint", "", "the Worker key that is to leave (worker_fingerprint of minion worker status --json; \"\" for none)")
	cmd.Flags().Uint64Var(&o.expectEpoch, "expect-enrollment-epoch", 0, "the enrollment that is to end (enrollment_epoch of minion worker status --json)")
	cmd.Flags().IntVar(&o.acceptLoss, "accept-loss", 0, "accept that up to this many sessions end (ConPTY)")
	cmd.Flags().BoolVar(&o.acceptUnknown, "accept-unknown-loss", false, "accept that an unknown number of sessions end")
	cmd.Flags().BoolVar(&o.json, "json", false, "print one JSON result object")
	addNamedSettingFlags(cmd, config.Default, config.KeyStateDir)
	return cmd
}

func runWorkerLeave(cmd *cobra.Command, deps workerLeaveDeps, o leaveOptions) (leaveResult, error) {
	res := leaveResult{StepsDone: []string{}, StillListedOnHub: true}
	expectFP := cmd.Flags().Changed("expect-worker-fingerprint")
	expectEpoch := cmd.Flags().Changed("expect-enrollment-epoch")
	switch {
	case o.yes && (!expectFP || !expectEpoch):
		return res, usagef("--yes needs --expect-worker-fingerprint FP and --expect-enrollment-epoch E (worker_fingerprint and enrollment_epoch of minion worker status --json)")
	case !o.yes && o.json:
		return res, usagef("--json asks no question: give --yes with --expect-worker-fingerprint and --expect-enrollment-epoch")
	case o.acceptLoss < 0:
		return res, usagef("--accept-loss takes a number of sessions, 0 or more")
	}
	ctx := cmdContext(cmd)

	// 1. Resolve the state directory once; everything below uses it.
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
	if dir.Value == "" {
		return res, usagef("setting %q is empty; give a state directory", config.KeyStateDir)
	}
	stateDir, err := filepath.Abs(dir.Value)
	if err != nil {
		return res, err
	}
	res.StateDir = stateDir

	fp, epoch, err := leaveIdentity(ctx, stateDir)
	if err != nil {
		return res, err
	}
	if !o.yes {
		ok, err := confirmLeave(cmd.OutOrStdout(), deps.stdin, stateDir, r, fp)
		if err != nil {
			return res, err
		}
		if !ok {
			return res, errors.New("leave cancelled: nothing was changed (use --yes to skip the question)")
		}
		if !expectFP {
			o.expectFP = fp
		}
		if !expectEpoch {
			o.expectEpoch = epoch
		}
	}
	if err := leaveExpect(o, fp, epoch, ""); err != nil {
		return res, err
	}
	res.StepsDone = append(res.StepsDone, leaveStepVerified)
	if _, err := os.Stat(stateDir); errors.Is(err, fs.ErrNotExist) {
		res.Left = true
		res.Note = leaveStillListed
		return res, nil
	}

	// 2. Stop that instance, with the session-loss preflight, and wait until
	// its lifecycle lock is free.
	stopped, lost, err := leaveStop(cmd, deps, o, stateDir)
	res.SessionsLost = lost
	if err != nil {
		return res, err
	}
	res.Stopped = stopped
	res.StepsDone = append(res.StepsDone, leaveStepStopped)

	// 3. Under the lifecycle and the enrollment locks: the same Worker key
	// and the same enrollment, then the reset.
	release, err := lockLifecycleAndEnrollment(ctx, stateDir, deps.lockState, deps.lockEnroll, 30*time.Second)
	if errors.Is(err, agent.ErrStateInUse) {
		return res, newResultError(ExitStateChanged, fmt.Sprintf("a Worker started again on %s after it was stopped; nothing was deleted", stateDir))
	}
	if err != nil {
		return res, err
	}
	defer release()
	fp, epoch, err = leaveIdentity(ctx, stateDir)
	if err != nil {
		return res, err
	}
	if err := leaveExpect(o, fp, epoch, "while the Worker was stopped"); err != nil {
		return res, err
	}
	st, closer, err := deps.openStore(ctx, stateDir)
	if errors.Is(err, store.ErrSchemaMismatch) {
		return res, &resultError{Exit: ExitSchemaMismatch, Message: err.Error() + "; the Worker was stopped, nothing was deleted", Err: err}
	}
	if err != nil {
		return res, fmt.Errorf("open the Worker state in %s: %w", stateDir, err)
	}
	defer func() { _ = closer.Close() }()
	rep, err := agent.ResetWorker(ctx, agent.ResetOptions{
		StateDir:   stateDir,
		ConfigPath: filepath.Join(stateDir, workerConfigName),
		Store:      st,
	})
	res.StepsDone = append(res.StepsDone, rep.Steps...)
	res.Removed, res.Kept, res.PauseCleared = rep.ConfigKeys, rep.Kept, rep.PauseCleared
	if err != nil {
		return res, &resultError{Exit: ExitError, Err: err, Message: fmt.Sprintf("leaving the Hub stopped half way (%s); done: %s. Run the same command again to finish",
			oneLine(err.Error()), strings.Join(res.StepsDone, ", "))}
	}
	res.NotRemoved = leaveEnvPins(deps.lookupEnv)
	res.Left = true
	res.Note = leaveStillListed
	return res, nil
}

// leaveIdentity reads the Worker key fingerprint ("" when there is no key)
// and the enrollment generation of stateDir.
func leaveIdentity(ctx context.Context, stateDir string) (fp string, epoch uint64, err error) {
	k, err := identity.LoadKey(filepath.Join(stateDir, agent.WorkerKeyFile))
	switch {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		return "", 0, fmt.Errorf("read the Worker key in %s: %w", stateDir, err)
	default:
		fp = k.Fingerprint().String()
	}
	epoch, err = agent.ReadEnrollmentEpoch(ctx, stateDir)
	if err != nil {
		return "", 0, err
	}
	return fp, epoch, nil
}

// leaveExpect compares the expectations with the Worker key and enrollment
// generation found: exit 27 when either changed.
func leaveExpect(o leaveOptions, fp string, epoch uint64, when string) error {
	if when != "" {
		when = " " + when
	}
	switch {
	case o.expectFP != fp:
		return newResultError(ExitStateChanged, fmt.Sprintf("the Worker key changed%s (it is %s, expected %s); nothing was deleted",
			when, orNone(fp), orNone(o.expectFP)))
	case o.expectEpoch != epoch:
		return newResultError(ExitStateChanged, fmt.Sprintf("the enrollment changed%s (enrollment_epoch is %d, expected %d): the Worker joined or left a Hub meanwhile; nothing was deleted",
			when, epoch, o.expectEpoch))
	}
	return nil
}

func orNone(s string) string {
	if s == "" {
		return "none"
	}
	return s
}

// leaveStop stops the Worker of stateDir and waits until its lifecycle lock
// is free. A Worker of this user that runs on another state directory is left
// alone.
func leaveStop(cmd *cobra.Command, deps workerLeaveDeps, o leaveOptions, stateDir string) (stopped bool, lost *int, err error) {
	if leaveStateFree(deps, stateDir) {
		return false, nil, nil // nothing runs on it
	}
	ctx := cmdContext(cmd)
	var reply agent.LiveStatusReply
	live := false
	if deps.down.call != nil {
		cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		raw, serr := deps.down.call(cctx, agent.OpLocalStatus)
		cancel()
		live = serr == nil && json.Unmarshal(raw, &reply) == nil
	}
	if live && reply.StateDir != "" && !sameDir(reply.StateDir, stateDir) {
		return false, nil, fmt.Errorf("a Worker holds the state directory %s, but the Worker that answers this user's control socket uses %s; stop the Worker of %s first; nothing was changed",
			stateDir, reply.StateDir, stateDir)
	}
	dres, err := runWorkerDownOpts(cmd, deps.down, downOptions{acceptLoss: o.acceptLoss, acceptUnknown: o.acceptUnknown, json: true})
	if err != nil {
		var re *resultError
		if errors.As(err, &re) {
			// 23, 29, 21 and 27 keep their status; nothing was deleted.
			re.Message += "; nothing was deleted"
			return false, dres.SessionsLost, re
		}
		return false, dres.SessionsLost, fmt.Errorf("%w; nothing was deleted", err)
	}
	wait := deps.stopWait
	for start := time.Now(); !leaveStateFree(deps, stateDir); {
		if time.Since(start) > wait {
			return dres.Stopped, dres.SessionsLost, fmt.Errorf("the Worker of %s was asked to stop but still holds its state after %s; check it with minion worker status --state-dir %s; nothing was deleted",
				stateDir, wait, stateDir)
		}
		sleep := deps.sleep
		if sleep == nil {
			sleep = time.Sleep
		}
		sleep(100 * time.Millisecond)
	}
	return true, dres.SessionsLost, nil
}

// leaveStateFree reports whether no process holds the lifecycle lock of
// stateDir.
func leaveStateFree(deps workerLeaveDeps, stateDir string) bool {
	if deps.lockState == nil {
		return true
	}
	l, err := deps.lockState(stateDir)
	if err != nil {
		return false
	}
	_ = l.Close()
	return true
}

// leaveEnvPins lists the Hub settings that environment variables still set:
// they are not stored and leave cannot remove them.
func leaveEnvPins(lookupEnv func(string) (string, bool)) []leaveLeft {
	if lookupEnv == nil {
		return nil
	}
	keys := append(append([]string(nil), agent.ResetConfigKeys...), config.KeyHubFingerprint)
	var left []leaveLeft
	for _, k := range keys {
		s, ok := config.Default.Lookup(k)
		if !ok {
			continue
		}
		for _, name := range []string{s.EnvName(), s.FileEnvName()} {
			if v, ok := lookupEnv(name); ok && strings.TrimSpace(v) != "" {
				left = append(left, leaveLeft{Key: k, Source: "env", Name: name})
			}
		}
	}
	return left
}

// confirmLeave says what leave deletes and reads one answer. Anything but y
// or yes (also the end of input) declines.
func confirmLeave(w io.Writer, in io.Reader, stateDir string, r *config.Resolver, fp string) (bool, error) {
	hub := "none"
	if v, ok, _ := fileValue(r.File, config.KeyServer); ok && v != "" {
		hub = safeURL(v)
	}
	worker := "none (no key)"
	if fp != "" {
		worker = identity.Fingerprint(fp).Words()
	}
	_, err := fmt.Fprintf(w, `This makes the Worker whose state is in %s leave its Hub.
  Hub:    %s
  Worker: %s
It stops that Worker (sessions that live inside it, Windows ConPTY, end) and deletes:
  - the Worker key pair (a new Hub sees a new Worker)
  - the pinned Hub key, the enrollment and the Hub address
  - the share selection and the local policy
It keeps your tmux sessions, the session counter and allow_new_sessions.
The Worker stays listed on the Hub until you remove it there.
Continue? [y/N] `, stateDir, hub, worker)
	if err != nil {
		return false, err
	}
	if in == nil {
		return false, nil
	}
	line, err := bufio.NewReader(in).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return false, err
	}
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		return true, nil
	}
	return false, nil
}

func printLeaveReport(w io.Writer, res leaveResult) {
	if res.Stopped {
		_, _ = fmt.Fprintln(w, "The Worker was stopped.")
	}
	if res.SessionsLost != nil && *res.SessionsLost > 0 {
		_, _ = fmt.Fprintf(w, "%s sessions ended with it.\n", strconv.Itoa(*res.SessionsLost))
	}
	_, _ = fmt.Fprintf(w, "Left the Hub: the key pair, the pin and enrollment, the share selection and the local policy are deleted (state %s).\n", res.StateDir)
	if len(res.Kept) > 0 {
		_, _ = fmt.Fprintf(w, "Kept in the config file (not set by a join, or changed since): %s.\n", strings.Join(res.Kept, ", "))
	}
	for _, l := range res.NotRemoved {
		_, _ = fmt.Fprintf(w, "Still set by the environment variable %s (%s): remove it there.\n", l.Name, l.Key)
	}
	_, _ = fmt.Fprintln(w, "Your tmux sessions, the session counter and allow_new_sessions were not touched.")
	_, _ = fmt.Fprintln(w, "The Worker stays listed on the Hub until you remove it there.")
}
