package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/patrick-1984/ai-minion/internal/config"
	"github.com/patrick-1984/ai-minion/internal/worker/agent"
	"github.com/patrick-1984/ai-minion/internal/worker/localapi"
	"github.com/patrick-1984/ai-minion/internal/worker/store"
)

func init() {
	Register(func(root *cobra.Command) {
		workerCommand(root).AddCommand(newWorkerResetCmd(defaultWorkerResetDeps()))
	})
}

// workerResetDeps are the inputs of "minion worker reset" that tests replace.
type workerResetDeps struct {
	stdin      io.Reader
	lookupEnv  func(string) (string, bool)
	loadConfig func(stateDir string) (config.FileSource, error)
	// workerRunning returns the control socket that a running Worker of this
	// user answers on, if any. It is an early, friendly check (and the only
	// one that sees a Worker from before the lifecycle lock existed); the
	// lock is what makes the reset safe.
	workerRunning func() (socket string, running bool)
	// lockState takes the exclusive lifecycle lock of the state directory
	// without waiting: the lock that every running Worker holds from before
	// it opens its state, on every platform, with or without a control
	// socket. A reset runs in another process and cannot make a running
	// Worker drop its authenticated connection, so it refuses while one
	// runs, and holds the lock from the end of the confirmation until the
	// last file is deleted so that no Worker starts in between. It returns
	// an error that wraps agent.ErrStateInUse when the lock is taken.
	lockState func(stateDir string) (io.Closer, error)
	// openStore opens the Worker state of the directory.
	openStore func(ctx context.Context, stateDir string) (agent.ResetStore, io.Closer, error)
}

func defaultWorkerResetDeps() workerResetDeps {
	return workerResetDeps{
		stdin:      os.Stdin,
		lookupEnv:  os.LookupEnv,
		loadConfig: loadWorkerConfig,
		workerRunning: func() (string, bool) {
			return liveControlSocket(config.SystemPathEnv(), time.Second)
		},
		lockState: agent.LockLifecycle,
		openStore: func(ctx context.Context, dir string) (agent.ResetStore, io.Closer, error) {
			st, err := store.OpenContext(ctx, dir)
			if err != nil {
				return nil, nil, err
			}
			return st, st, nil
		},
	}
}

func newWorkerResetCmd(deps workerResetDeps) *cobra.Command {
	var yes bool
	cmd := &cobra.Command{
		Use:   "reset",
		Short: "Forget the Hub and return this Worker to the unconfigured state",
		Long: `Reset ends the pairing of this Worker from the machine's side.

It deletes the Worker's key pair, the pinned Hub key and the enrollment, the
share selection and the local policy (preview_only, share_existing and the Hub
settings of the config file), so that the next "minion worker up" runs the
first-run wizard. Your tmux sessions and the session counter are not touched.

A pairing otherwise ends only when a user removes the Worker in the Hub. The
Hub is not told about a reset: remove the Worker there too, because a
re-enrollment creates a new Worker with a new key.

Stop the Worker service first: reset refuses while a Worker runs (it holds the
lock <state>/worker.lock, on every platform), because a running Worker would
keep its authenticated connection to the Hub. While reset deletes, a Worker
cannot start. Settings that come from flags or environment variables
are not stored and stay in effect.

Without --yes the command lists what it deletes and asks for confirmation.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runWorkerReset(cmd, deps, yes)
		},
	}
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "do not ask for confirmation")
	if s, ok := config.Default.Lookup(config.KeyStateDir); ok && s.HasFlag() {
		cmd.Flags().String(s.FlagName(), "", fmt.Sprintf("%s (env %s)", s.Help, s.EnvName()))
	}
	return cmd
}

func runWorkerReset(cmd *cobra.Command, deps workerResetDeps, yes bool) error {
	res := &config.Resolver{
		Role:      config.ScopeWorker,
		Flags:     changedFlags{cmd: cmd, reg: config.Default},
		LookupEnv: deps.lookupEnv,
	}
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
	if dir.Value == "" {
		return fmt.Errorf("setting %q is empty; give a state directory", config.KeyStateDir)
	}
	stateDir, err := filepath.Abs(dir.Value)
	if err != nil {
		return err
	}
	out := cmd.OutOrStdout()
	if _, err := os.Stat(stateDir); errors.Is(err, os.ErrNotExist) {
		_, err = fmt.Fprintf(out, "Nothing to reset: %s does not exist, this Worker is already unconfigured.\n", stateDir)
		return err
	}

	if deps.workerRunning != nil {
		if sock, running := deps.workerRunning(); running {
			return fmt.Errorf("a Worker is running (it answers on %s) and would keep its connection to the Hub: stop it first, then run reset again; nothing was changed", sock)
		}
	}
	// Early check, before the question: a running Worker is no reason to ask.
	// The lock is released again, because the question may wait for a long
	// time and a stopped service must be able to restart meanwhile.
	if err := probeState(deps, stateDir); err != nil {
		return err
	}

	if !yes {
		ok, err := confirmReset(out, deps.stdin, stateDir)
		if err != nil {
			return err
		}
		if !ok {
			return errors.New("reset cancelled: nothing was changed (use --yes to skip the question)")
		}
	}

	// The check that counts: a Worker may have started while the question
	// waited. From here until the reset ends no Worker can take the lock.
	lock, err := lockState(deps, stateDir)
	if err != nil {
		return err
	}
	defer func() { _ = lock.Close() }()

	st, closer, err := deps.openStore(cmd.Context(), stateDir)
	if err != nil {
		return fmt.Errorf("open the Worker state in %s: %w", stateDir, err)
	}
	defer func() { _ = closer.Close() }()
	rep, err := agent.ResetWorker(cmd.Context(), agent.ResetOptions{
		StateDir:   stateDir,
		ConfigPath: filepath.Join(stateDir, workerConfigName),
		Store:      st,
	})
	if err != nil {
		return err
	}
	printResetReport(out, stateDir, rep)
	return nil
}

// probeState checks that no process holds the lifecycle lock and lets go of it.
func probeState(deps workerResetDeps, stateDir string) error {
	lock, err := lockState(deps, stateDir)
	if err != nil {
		return err
	}
	return lock.Close()
}

// lockState takes the lifecycle lock through the dependency; a nil dependency
// means no lock (a test that does not care).
func lockState(deps workerResetDeps, stateDir string) (io.Closer, error) {
	if deps.lockState == nil {
		return nopCloser{}, nil
	}
	lock, err := deps.lockState(stateDir)
	if errors.Is(err, agent.ErrStateInUse) {
		return nil, fmt.Errorf("a Worker is running on %s (or another reset is in progress) and would keep its connection to the Hub: stop it first, then run reset again; nothing was changed", stateDir)
	}
	if err != nil {
		return nil, fmt.Errorf("lock the Worker state in %s: %w", stateDir, err)
	}
	return lock, nil
}

type nopCloser struct{}

func (nopCloser) Close() error { return nil }

// confirmReset lists what the reset deletes and reads one answer. Anything but
// y or yes (including end of input, as in a script without --yes) declines.
func confirmReset(w io.Writer, in io.Reader, stateDir string) (bool, error) {
	_, err := fmt.Fprintf(w, `This resets the Worker whose state is in %s.
It deletes:
  - the Worker key pair (a new Hub sees a new Worker)
  - the pinned Hub key and the enrollment
  - the share selection and the local policy
It keeps your tmux sessions and the session counter.
Continue? [y/N] `, stateDir)
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

func printResetReport(w io.Writer, stateDir string, rep agent.ResetReport) {
	if rep.Nothing() {
		_, _ = fmt.Fprintf(w, "Nothing to reset: the Worker in %s is already unconfigured.\n", stateDir)
		return
	}
	_, _ = fmt.Fprintf(w, "Worker reset: the key pair, the pin and enrollment, the share selection and the local policy are deleted (state %s).\n", stateDir)
	_, _ = fmt.Fprintln(w, "Your tmux sessions and the session counter were not touched.")
	_, _ = fmt.Fprintln(w, "The next \"minion worker up\" runs the first-run wizard.")
	if rep.WasEnrolled {
		_, _ = fmt.Fprintln(w, "The Hub still lists this Worker: remove it there.")
	}
}

// liveControlSocket reports the control socket of a running Worker of this
// user: the first candidate path that accepts a connection. A missing or
// stale socket file does not count. On Windows the path is the named pipe of
// the user. Where neither exists the dial fails and no Worker is found.
func liveControlSocket(env config.PathEnv, timeout time.Duration) (string, bool) {
	paths, err := localapi.SocketCandidates(env)
	if err != nil {
		return "", false
	}
	for _, p := range paths {
		conn, err := localapi.Dial(p, timeout)
		if err == nil {
			_ = conn.Close()
			return p, true
		}
	}
	return "", false
}
