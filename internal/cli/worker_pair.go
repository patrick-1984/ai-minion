package cli

import (
	"bufio"
	"context"
	"encoding/json"
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
	"github.com/patrick-1984/ai-minion/internal/proto"
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
	// saveServer remembers the Hub URL of "join" and the expected Hub
	// fingerprint ("" for none) in the Worker config file, in one write; nil
	// means the file in the state directory.
	saveServer func(stateDir, url, fingerprint string) error
	// interactive reports whether the command may ask (a terminal on both
	// ends); nil means the real check.
	interactive func(in io.Reader, out io.Writer) bool
	// inviteDialer is the enroll_invite transport of "join --token-file";
	// nil means agent.NewInviteDialer().
	inviteDialer agent.InviteDialer
	// call asks the running Worker over the local API (status, restart,
	// share); nil means the control socket of this user.
	call func(ctx context.Context, op string, args any) (json.RawMessage, error)
	// restartSleep and restartWait tune the wait for the restarted Worker;
	// nil and zero mean the defaults of "worker restart".
	restartSleep func(time.Duration)
	restartWait  time.Duration
}

func defaultWorkerPairDeps() workerPairDeps {
	return workerPairDeps{
		stdin:      os.Stdin,
		stdout:     os.Stdout,
		stderr:     os.Stderr,
		lookupEnv:  os.LookupEnv,
		loadConfig: loadWorkerConfig,
		openStore: func(ctx context.Context, dir string) (*store.Store, error) {
			return store.OpenContext(ctx, dir, store.NoMigrate())
		},
		saveServer: saveWorkerHub,
	}
}

// pairOptions are the flags of the pairing commands.
type pairOptions struct {
	newCode bool
	yes     bool
	// join is the Hub URL argument of "join"; empty for "pair".
	join string
	// The staged join with a join token (tray W7a): --token-file, --resume,
	// --json, --accept-loss and --accept-unknown-loss.
	tokenFile     string
	resume        bool
	asJSON        bool
	acceptLoss    int
	acceptUnknown bool
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
		Args: noArgsRedacted,
		RunE: withHubLine(func(cmd *cobra.Command, _ []string) error {
			return runWorkerPair(cmd, deps, opt)
		}, deps.lookupEnv),
	}
	cmd.Flags().BoolVar(&opt.newCode, "new-code", false, "pair again with a new Worker key (asks first when the Worker is paired)")
	cmd.Flags().BoolVar(&opt.yes, "yes", false, "do not ask for confirmation")
	addSettingFlags(cmd, config.Default, config.ScopeWorker)
	return cmd
}

func newWorkerJoinCmd(deps workerPairDeps) *cobra.Command {
	var opt pairOptions
	cmd := &cobra.Command{
		Use:   "join <url> | join --token-file PATH|- | join --resume",
		Short: "Join a Hub: pair with a code, or enroll with a join token",
		Long: `Two ways to join a Hub.

minion worker join <url>
  Remember the Hub address in the Worker's config file and start pairing with
  it: the same as "minion worker pair --server <url>", but the address is
  kept, so that the Worker service connects to this Hub afterwards. A Worker
  service that is already running keeps the address it started with: restart
  it. A paired Worker refuses with already_paired and the address is not
  changed.

minion worker join --token-file PATH|- [--share-existing all|none|later] [--json]
  Enroll with a join token (mnj1-... from the Hub's Add worker page), read
  from the file PATH or, with "-", from stdin; the token is never an argument.
  The join runs in three stages and reports each one:
    enrollment  committed | already   the Worker is enrolled with the token's Hub
    config      saved | failed        server and hub_fingerprint are in the config file
    activation  restarted | not_running | failed
                                      the running Worker of this state directory restarted
  It exits 0 only when all three are done: 24 when no Worker runs (start it
  with minion worker start), 23 or 29 when the restart would end sessions
  (repeat with --resume --accept-loss N), 1 when the config could not be saved.
  The first join of a Worker that has no sharing choice needs
  --share-existing; the choice is saved before the Worker enrolls. A paused
  Worker refuses (exit 25). A token whose Hub differs from the server or
  hub_fingerprint already saved or given is refused before anything is sent.

minion worker join --resume [--accept-loss N] [--json]
  Finish an unfinished join without the token: check that the stored
  enrollment was made with the saved (or given) Hub address and fingerprint
  (exit 27 when it was not), then save the config and restart the Worker. It
  never contacts the Hub.`,
		Args: cobra.MaximumNArgs(1),
		RunE: withHubLine(func(cmd *cobra.Command, args []string) error {
			if opt.tokenFile != "" || opt.resume || opt.asJSON || cmd.Flags().Changed("accept-loss") || opt.acceptUnknown {
				return runWorkerJoinStaged(cmd, deps, opt, args)
			}
			if len(args) != 1 {
				return usagef("give the Hub address (minion worker join <url>), or a join token with --token-file PATH|-")
			}
			opt.join = args[0]
			return runWorkerPair(cmd, deps, opt)
		}, deps.lookupEnv),
	}
	cmd.Flags().BoolVar(&opt.newCode, "new-code", false, "pair again with a new Worker key (asks first when the Worker is paired)")
	cmd.Flags().BoolVar(&opt.yes, "yes", false, "do not ask for confirmation")
	cmd.Flags().StringVar(&opt.tokenFile, "token-file", "", "enroll with the join token in this file (- reads it from stdin)")
	cmd.Flags().BoolVar(&opt.resume, "resume", false, "finish an unfinished join (config and restart) without the token")
	cmd.Flags().BoolVar(&opt.asJSON, "json", false, "print one JSON result object (with --token-file or --resume)")
	cmd.Flags().IntVar(&opt.acceptLoss, "accept-loss", 0, "accept that up to this many sessions end when the Worker restarts (ConPTY)")
	cmd.Flags().BoolVar(&opt.acceptUnknown, "accept-unknown-loss", false, "accept that an unknown number of sessions end when the Worker restarts")
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
		if identity.LooksLikeJoinToken(opt.join) {
			// Never echo it: the token holds an invitation key.
			return errors.New("this is a join token, not a Hub address: " + joinHint)
		}
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
	stderr := deps.stderr
	if stderr == nil {
		stderr = cmd.ErrOrStderr()
	}
	logger, err := commandLogger(cmd, res, stderr)
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
	// A paused Worker talks to no Hub, and pairing is Hub traffic (tray W3c).
	if p, err := agent.ReadPause(core.StateDir); err != nil {
		return err
	} else if p.Active(time.Now()) {
		_, _ = fmt.Fprintln(stderr, "Error: this Worker is paused, so it talks to no Hub; resume it first (minion worker resume), then run this again. Nothing was changed.")
		return &exitCodeError{ExitPaused}
	}

	ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// The questions (the fingerprint, the confirmation of --new-code) come
	// before the state is opened for writing: opening it migrates the
	// database, and a cancelled command must leave a state that an older
	// installed Worker can still open. The preflight reads it read-only.
	paired, isPaired, err := readPairing(ctx, core.StateDir)
	if err != nil {
		return err
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
	// The expected Hub fingerprint: the setting, else the prompt in a
	// terminal. Both come before any network probe and before anything is
	// changed on disk (C2); a malformed setting is an error, never "absent".
	expect, err := agent.ParseHubExpectation(hub.Fingerprint)
	if err != nil {
		return err
	}
	interactive := deps.interactive
	if interactive == nil {
		interactive = func(in io.Reader, out io.Writer) bool { return isTerminalStream(in) && isTerminalWriter(out) }
	}
	if expect.IsZero() && interactive(deps.stdin, stdout) {
		if expect, err = agent.AskHubFingerprint(deps.stdin, stdout, hub.Server); err != nil {
			return err
		}
	}
	// A paired Worker is asked first. An unpaired one is not: nothing is
	// lost, and its key may be one the Hub removed (revoked), which only a
	// new key can leave.
	if opt.newCode && isPaired && !opt.yes {
		if err := confirmNewCode(deps.stdin, stdout, paired); err != nil {
			return err
		}
	}

	open := deps.openStore
	if open == nil {
		open = func(ctx context.Context, dir string) (*store.Store, error) {
			return store.OpenContext(ctx, dir, store.NoMigrate())
		}
	}
	st, err := open(ctx, core.StateDir)
	if err != nil {
		return fmt.Errorf("open the Worker state in %s: %w", core.StateDir, err)
	}
	defer func() { _ = st.Close() }()
	enr := agent.NewEnrollments(st, core.StateDir)

	// The answers above were given for the pairing read in the preflight;
	// another one stored meanwhile is not replaced on them.
	now, nowPaired, err := enr.Load(ctx)
	if err != nil {
		return fmt.Errorf("read the enrollment: %w", err)
	}
	if nowPaired != isPaired || (isPaired && !now.Pin.Pub.Equal(paired.Pin.Pub)) {
		return errors.New("the pairing of this Worker changed while the command ran; nothing was changed, run it again")
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
		ExpectHub:  expect,
		Out:        stdout,
		Logger:     logger,
	})
	if err != nil {
		return err
	}
	if opt.newCode {
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
			save = saveWorkerHub
		}
		if err := save(core.StateDir, hub.Server, expect.String()); err != nil {
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
// readPairing reads the enrollment of the state in stateDir without creating
// or migrating the database (none means not paired).
func readPairing(ctx context.Context, stateDir string) (agent.Enrollment, bool, error) {
	if _, err := os.Stat(filepath.Join(stateDir, store.DBFile)); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return agent.Enrollment{}, false, nil
		}
		return agent.Enrollment{}, false, fmt.Errorf("look at the Worker state in %s: %w", stateDir, err)
	}
	st, err := store.OpenReadOnlyContext(ctx, stateDir)
	if err != nil {
		return agent.Enrollment{}, false, fmt.Errorf("open the Worker state in %s: %w", stateDir, err)
	}
	defer func() { _ = st.Close() }()
	enr, ok, err := agent.NewEnrollments(st, stateDir).Load(ctx)
	switch {
	case err != nil && strings.Contains(err.Error(), "no such table"):
		return agent.Enrollment{}, false, nil // a state of a release before pairing
	case err != nil:
		return agent.Enrollment{}, false, fmt.Errorf("read the enrollment: %w", err)
	}
	return enr, ok, nil
}

func confirmNewCode(in io.Reader, out io.Writer, enr agent.Enrollment) error {
	name := enr.Name
	if name == "" {
		name = "this Worker"
	}
	owner := ""
	if enr.Owner != "" {
		owner = " (paired to " + enr.Owner + ")"
	}
	name, owner = proto.SafeText(name, 255), proto.SafeText(owner, 160)
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

// saveWorkerHub stores the Hub URL as the server setting in the Worker config
// file of the state directory, and with it, in the same write, the expected
// Hub fingerprint (hub_fingerprint) when one was given, so that the Worker
// service applies the same rule when it pairs or resumes by itself. The read-modify-write runs under the same
// config lock as `minion worker config set|unset|share` and the cleanup of the
// invitation key, so that a change made meanwhile is not lost; the state
// directory is created first, like those commands do.
func saveWorkerHub(stateDir, url, fingerprint string) error {
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
	if fingerprint != "" {
		f.Set(config.KeyHubFingerprint, fingerprint)
	}
	return f.Save()
}
