package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/patrick-1984/ai-minion/internal/config"
	"github.com/patrick-1984/ai-minion/internal/identity"
	"github.com/patrick-1984/ai-minion/internal/logx"
	"github.com/patrick-1984/ai-minion/internal/transport"
	"github.com/patrick-1984/ai-minion/internal/worker/agent"
	"github.com/patrick-1984/ai-minion/internal/worker/store"
)

func init() {
	Register(func(root *cobra.Command) {
		worker := workerCommand(root)
		deps := defaultWorkerPairDeps()
		worker.AddCommand(newWorkerPairCmd(deps), newWorkerJoinCmd(deps))
	})
}

// workerPairDeps are the inputs of "minion worker pair" and "minion worker
// join" that tests replace.
type workerPairDeps struct {
	stdin      io.Reader
	stdout     io.Writer
	stderr     io.Writer
	lookupEnv  func(string) (string, bool)
	loadConfig func(stateDir string) (config.FileSource, error)
	openStore  func(ctx context.Context, stateDir string) (*store.Store, error)
	// dialer is the pairing transport; nil means agent.NewPairDialer().
	dialer agent.PairDialer
	// hubDialer is the connect transport that revokes the old identity of
	// --new-code at its Hub; nil means agent.NewHubDialer().
	hubDialer agent.Dialer
	// revokeWait bounds the first, synchronous attempt to revoke the old
	// identity; zero means 10 seconds.
	revokeWait time.Duration
	// clock and backoff are the pairer's time source and retry delays; nil
	// means the system clock and the default backoff.
	clock   transport.Clock
	backoff *transport.Backoff
	// newCode creates codes; nil means identity.NewPairingCode.
	newCode func() identity.PairingCode
	// saveServer remembers the Hub URL of "join" in the Worker config file;
	// nil means the file in the state directory.
	saveServer func(stateDir, url string) error
}

func defaultWorkerPairDeps() workerPairDeps {
	return workerPairDeps{
		stdin:      os.Stdin,
		stdout:     os.Stdout,
		stderr:     os.Stderr,
		lookupEnv:  os.LookupEnv,
		loadConfig: loadWorkerConfig,
		openStore:  func(ctx context.Context, dir string) (*store.Store, error) { return store.OpenContext(ctx, dir) },
		saveServer: saveWorkerServer,
	}
}

// pairOptions are the flags of the pairing commands.
type pairOptions struct {
	newCode bool
	yes     bool
	// join is the Hub URL argument of "join"; empty for "pair".
	join string
}

func newWorkerPairCmd(deps workerPairDeps) *cobra.Command {
	var opt pairOptions
	cmd := &cobra.Command{
		Use:   "pair",
		Short: "Show a pairing code to type into the Hub",
		Long: `Pair this Worker with the Hub: the Worker shows a code like MN-K7Q4-XY12 and
waits. Type the code into the Hub ("Add worker"); the Hub and the Worker then
confirm each other with it, and the Worker is added to your space. The code is
valid for 10 minutes; a fresh one is shown when it expires or the Hub rejects
it. The code is printed on the console and in the log, the only place a secret
is printed. Needs only the Hub address (--server, MINION_SERVER or the config
file); the Hub's TLS certificate is never consulted.

A paired Worker refuses with already_paired. With --new-code it creates a new
Worker key, wipes the old enrollment, revokes the old identity at its Hub (the
old key is kept in the state directory until the Hub confirmed, so an
unreachable Hub is retried by the Worker service) and pairs again, after a
confirmation unless --yes is given. A running Worker service drops its
connection to the old Hub at once.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runWorkerPair(cmd, deps, opt)
		},
	}
	cmd.Flags().BoolVar(&opt.newCode, "new-code", false, "pair again with a new Worker key (asks first when the Worker is paired)")
	cmd.Flags().BoolVar(&opt.yes, "yes", false, "do not ask for confirmation")
	addSettingFlags(cmd, config.Default, config.ScopeWorker)
	return cmd
}

func newWorkerJoinCmd(deps workerPairDeps) *cobra.Command {
	var opt pairOptions
	cmd := &cobra.Command{
		Use:   "join <url>",
		Short: "Remember the Hub address and pair with a code",
		Long: `Remember the Hub address in the Worker's config file and start pairing with it:
the same as "minion worker pair --server <url>", but the address is kept, so
that the Worker service connects to this Hub afterwards. A Worker service
that is already running keeps the address it started with: restart it. A
paired Worker refuses with already_paired and the address is not changed.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			opt.join = args[0]
			return runWorkerPair(cmd, deps, opt)
		},
	}
	cmd.Flags().BoolVar(&opt.newCode, "new-code", false, "pair again with a new Worker key (asks first when the Worker is paired)")
	cmd.Flags().BoolVar(&opt.yes, "yes", false, "do not ask for confirmation")
	addSettingFlags(cmd, config.Default, config.ScopeWorker)
	return cmd
}

// overrideFlags is a flag source whose value for one key is fixed: the Hub URL
// argument of "join" wins over every other source, like a flag does.
type overrideFlags struct {
	base  config.FlagSource
	key   string
	value string
}

func (o overrideFlags) Lookup(key string) (string, bool, error) {
	if key == o.key {
		return o.value, true, nil
	}
	return o.base.Lookup(key)
}

// runWorkerPair resolves the settings, refuses a paired Worker (or renews its
// identity for --new-code) and runs the pairing code lifecycle.
func runWorkerPair(cmd *cobra.Command, deps workerPairDeps, opt pairOptions) error {
	var flags config.FlagSource = changedFlags{cmd: cmd, reg: config.Default}
	if opt.join != "" {
		url, err := config.ValidateServerURL(opt.join)
		if err != nil {
			return err
		}
		if url == "" {
			return errors.New("give the Hub address, for example https://hub.example:8443")
		}
		flags = overrideFlags{base: flags, key: config.KeyServer, value: url}
	}
	res := &config.Resolver{Role: config.ScopeWorker, Flags: flags, LookupEnv: deps.lookupEnv}
	if deps.loadConfig != nil {
		file, err := loadConfigFile(res, deps.loadConfig)
		if err != nil {
			return err
		}
		res.File = file
	}
	level, err := res.Resolve(config.KeyLogLevel)
	if err != nil {
		return err
	}
	format, err := res.Resolve(config.KeyLogFormat)
	if err != nil {
		return err
	}
	stderr := deps.stderr
	if stderr == nil {
		stderr = cmd.ErrOrStderr()
	}
	logger, err := logx.New(logx.Options{Level: level.Value, Format: logx.Format(format.Value), Output: stderr})
	if err != nil {
		return err
	}
	core, err := config.ResolveWorkerCore(res)
	if err != nil {
		return err
	}
	hub, err := config.ResolveWorkerHub(res)
	if err != nil {
		return err
	}
	stdout := deps.stdout
	if stdout == nil {
		stdout = cmd.OutOrStdout()
	}

	ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	open := deps.openStore
	if open == nil {
		open = func(ctx context.Context, dir string) (*store.Store, error) { return store.OpenContext(ctx, dir) }
	}
	st, err := open(ctx, core.StateDir)
	if err != nil {
		return fmt.Errorf("open the Worker state in %s: %w", core.StateDir, err)
	}
	defer func() { _ = st.Close() }()
	enr := agent.NewEnrollments(st, core.StateDir)

	paired, isPaired, err := enr.Load(ctx)
	if err != nil {
		return fmt.Errorf("read the enrollment: %w", err)
	}
	if isPaired && !opt.newCode {
		return agent.AlreadyPaired(paired)
	}
	// Everything that can fail on the inputs happens before --new-code
	// destroys the key or the enrollment: a Worker whose Hub address is set
	// only in the service's environment keeps its pairing.
	if hub.Server == "" {
		return agent.ErrNoHubURL
	}
	dialer := deps.dialer
	if dialer == nil {
		dialer = agent.NewPairDialer()
	}
	p, err := agent.NewPairer(agent.PairerConfig{
		HubURL:     hub.Server,
		Name:       core.Name,
		Enrollment: enr,
		Dialer:     dialer,
		Clock:      deps.clock,
		Backoff:    deps.backoff,
		NewCode:    deps.newCode,
		Out:        stdout,
		Logger:     logger,
	})
	if err != nil {
		return err
	}
	if opt.newCode {
		// A paired Worker is asked first. An unpaired one is not: nothing
		// is lost, and its key may be one the Hub removed (revoked), which
		// only a new key can leave.
		if isPaired && !opt.yes {
			if err := confirmNewCode(deps.stdin, stdout, paired); err != nil {
				return err
			}
		}
		// The old key is revoked at the Hub recorded with the enrollment, not
		// at the address in the configuration or in a flag that only names
		// the Hub of the new pairing.
		if _, err := agent.RenewWorkerIdentity(ctx, enr); err != nil {
			return err
		}
		if isPaired {
			_, _ = fmt.Fprintln(stdout, "Created a new Worker key and removed the old pairing.")
			stopRevoke := revokeOldIdentity(ctx, deps, enr, core.Name, stdout, logger)
			defer stopRevoke()
		} else {
			_, _ = fmt.Fprintln(stdout, "Created a new Worker key.")
		}
	}
	if opt.join != "" {
		save := deps.saveServer
		if save == nil {
			save = saveWorkerServer
		}
		if err := save(core.StateDir, hub.Server); err != nil {
			return fmt.Errorf("remember the Hub address: %w", err)
		}
	}
	if _, err := p.Run(ctx); err != nil {
		if errors.Is(err, context.Canceled) && ctx.Err() != nil {
			_, _ = fmt.Fprintln(stdout, "Pairing stopped.")
			return nil
		}
		return err
	}
	_, _ = fmt.Fprintln(stdout, "The Worker service connects to the Hub with this pairing; start it with `minion worker run` if it is not running.")
	if opt.join != "" {
		// The service reads the Hub address once, when it starts: there is no
		// configuration reload and the control socket has no op to apply a
		// setting. A service that is already running keeps the address it
		// started with (none, for a Worker that ran alone).
		_, _ = fmt.Fprintf(stdout, "The address %s is saved. A Worker service that is already running uses the address it started with: restart it to connect to this Hub.\n", hub.Server)
	}
	return nil
}

// revokeOldIdentity makes the first attempt to revoke the replaced identities
// at their Hubs, waits for it for a short time, and, when a Hub could not be
// reached, keeps trying in the background while this command runs. The pending
// record stays on disk either way: the Worker service finishes the job. The
// returned function stops the background retries.
func revokeOldIdentity(ctx context.Context, deps workerPairDeps, enr *agent.Enrollments, name string, out io.Writer, logger *slog.Logger) (stop func()) {
	dialer := deps.hubDialer
	if dialer == nil {
		dialer = agent.NewHubDialer()
	}
	ret, err := agent.NewRetirer(agent.RetirerConfig{
		Store: enr.RetiredStore(), Dialer: dialer, Name: name, Clock: deps.clock, Backoff: deps.backoff, Logger: logger,
	})
	if err != nil {
		_, _ = fmt.Fprintf(out, "Could not start revoking the old identity: %v\n", err)
		return func() {}
	}
	wait := deps.revokeWait
	if wait <= 0 {
		wait = 10 * time.Second
	}
	first, cancelFirst := context.WithTimeout(ctx, wait)
	remaining, _ := ret.RunOnce(first)
	cancelFirst()
	if remaining == 0 {
		_, _ = fmt.Fprintln(out, "The old identity was revoked at the Hub.")
		return func() {}
	}
	_, _ = fmt.Fprintln(out, "The Hub could not revoke the old identity yet. It is kept until the Hub confirms: the Worker service retries\n"+
		"whenever it runs (`minion worker run`), and this command retries while it waits for the code.")
	bg, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { defer close(done); ret.Run(bg) }()
	return func() { cancel(); <-done }
}

// confirmNewCode asks before a paired Worker gets a new identity.
func confirmNewCode(in io.Reader, out io.Writer, enr agent.Enrollment) error {
	name := enr.Name
	if name == "" {
		name = "this Worker"
	}
	owner := ""
	if enr.Owner != "" {
		owner = " (paired to " + enr.Owner + ")"
	}
	_, _ = fmt.Fprintf(out, "%s%s is already paired. A new code creates a new Worker key and forgets the old pairing;\n"+
		"the old identity is revoked at the Hub. Continue? [y/N] ", name, owner)
	if in == nil {
		return errors.New("aborted: nothing was changed (use --yes to skip the question)")
	}
	line, _ := bufio.NewReader(in).ReadString('\n')
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		return nil
	}
	return errors.New("aborted: nothing was changed")
}

// saveWorkerServer stores the Hub URL as the server setting in the Worker
// config file of the state directory. The read-modify-write runs under the same
// config lock as `minion worker config set|unset|share` and the cleanup of the
// invitation key, so that a change made meanwhile is not lost; the state
// directory is created first, like those commands do.
func saveWorkerServer(stateDir, url string) error {
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return fmt.Errorf("cannot create the state directory %q: %w", stateDir, err)
	}
	release, err := lockConfigFile(filepath.Join(stateDir, configLockName))
	if err != nil {
		return err
	}
	defer release()
	f, err := config.LoadFile(filepath.Join(stateDir, workerConfigName))
	if err != nil {
		return err
	}
	f.Set(config.KeyServer, url)
	return f.Save()
}
