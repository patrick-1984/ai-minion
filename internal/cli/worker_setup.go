package cli

import (
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/patrick-1984/ai-minion/internal/config"
	"github.com/patrick-1984/ai-minion/internal/identity"
	"github.com/patrick-1984/ai-minion/internal/logx"
	"github.com/patrick-1984/ai-minion/internal/transport"
	"github.com/patrick-1984/ai-minion/internal/worker/agent"
	"github.com/patrick-1984/ai-minion/internal/worker/service"
	"github.com/patrick-1984/ai-minion/internal/worker/setup"
	"github.com/patrick-1984/ai-minion/internal/worker/store"
)

func init() {
	Register(func(root *cobra.Command) {
		worker := workerCommand(root)
		worker.AddCommand(newWorkerSetupCmd(defaultWorkerSetupDeps()))
	})
}

// setupStore is the part of the Worker state the wizard uses: the share choice
// and the enrollment. *store.Store implements it.
type setupStore interface {
	shareStore
	agent.EnrollmentRepo
}

// workerSetupDeps are the inputs of the first-run wizard that tests replace.
// The zero value uses the real thing for every field.
type workerSetupDeps struct {
	lookupEnv  func(string) (string, bool)
	loadConfig func(stateDir string) (config.FileSource, error)
	// checkTerminal checks the session host before a sharing question or enrollment.
	checkTerminal func(context.Context, *config.Resolver, string, *ownerSetup) error
	// interactive reports whether in and out are a terminal, so that the
	// wizard may ask questions; nil checks the real streams.
	interactive func(in io.Reader, out io.Writer) bool
	// openStore opens the Worker state; nil opens the real database.
	openStore func(ctx context.Context, stateDir string) (setupStore, error)
	// sessions returns the lister of the tmux sessions on socket; nil lists
	// them with tmux. A nil result means "cannot list".
	sessions func(socket string) setup.SessionLister
	// checker builds the Hub address check; nil builds the real one.
	checker func(hub config.WorkerHub) (setup.HubChecker, error)
	// dialer is the pairing transport; nil means agent.NewPairDialer().
	dialer agent.PairDialer
	// clock, backoff and newCode are the pairer's seams (see workerPairDeps).
	clock   transport.Clock
	backoff *transport.Backoff
	newCode func() identity.PairingCode
	// assumeSetUp skips the first-run gate of "worker up". It is for tests
	// of other behavior of "worker up" that need no state of a first run.
	assumeSetUp bool
	// runAsUser runs this binary as the target user (A57); nil runs it for real.
	runAsUser func(ctx context.Context, t service.Target, binary string, args []string, out io.Writer) error
}

func defaultWorkerSetupDeps() workerSetupDeps {
	return workerSetupDeps{lookupEnv: os.LookupEnv, loadConfig: loadWorkerConfig}
}

func (d workerSetupDeps) env() func(string) (string, bool) {
	if d.lookupEnv != nil {
		return d.lookupEnv
	}
	return os.LookupEnv
}

func (d workerSetupDeps) isInteractive(in io.Reader, out io.Writer) bool {
	if d.interactive != nil {
		return d.interactive(in, out)
	}
	return isTerminalStream(in) && logx.IsTerminal(out)
}

// isTerminalStream reports whether r is a terminal (a character device that is
// not the null device).
func isTerminalStream(r io.Reader) bool {
	f, ok := r.(*os.File)
	return ok && logx.IsTerminal(f)
}

func isTerminalWriter(w io.Writer) bool {
	f, ok := w.(*os.File)
	return ok && isTerminalStream(f)
}

func (d workerSetupDeps) open(ctx context.Context, stateDir string) (setupStore, error) {
	if d.openStore != nil {
		return d.openStore(ctx, stateDir)
	}
	return store.OpenContext(ctx, stateDir)
}

func newWorkerSetupCmd(deps workerSetupDeps) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "setup",
		Short: "Guided first-run setup: what to share, the Hub address and a pairing code",
		Long: `Set up this Worker in three short steps:

  1. What may the Hub see? It shows how many tmux sessions it sees and lets
     you share all of them (the default), choose some now, decide later, or
     share none. Sessions are never renamed, closed or changed.
  2. The Hub address. It checks the name (DNS), the connection (TCP), the
     certificate (TLS, with its fingerprint when the Hub uses its own
     certificate) and the protocol, and explains each result. If the check
     fails you can correct the address or save it anyway. Press Enter to skip
     it and run the Worker alone (Solo).
  3. The pairing code. It is shown here, with the instructions to type it into
     the Hub ("Add worker"), and the wizard ends with "paired to <user>" when
     the pairing completes while it is open. Press Enter to leave without
     waiting; "minion worker pair" shows a code later.

Nothing is saved before step 2 is done, and ending the wizard before then
(Ctrl-C) changes nothing.

Every question can be answered with a flag or an environment variable instead,
for example --share-existing all and --server https://hub.example:8443. A
question that is answered that way is not asked. Without a terminal the wizard
asks nothing: it needs --share-existing, and it does not pair.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runWorkerSetup(cmd, deps)
		},
	}
	addSettingFlags(cmd, config.Default, config.ScopeWorker)
	return cmd
}

func runWorkerSetup(cmd *cobra.Command, deps workerSetupDeps) error {
	res := &config.Resolver{
		Role:      config.ScopeWorker,
		Flags:     changedFlags{cmd: cmd, reg: config.Default},
		LookupEnv: deps.env(),
	}
	load := deps.loadConfig
	if load == nil {
		load = loadWorkerConfig
	}
	file, err := loadConfigFile(res, load)
	if err != nil {
		return err
	}
	res.File = file
	core, err := config.ResolveWorkerCore(res)
	if err != nil {
		return err
	}
	_, err = deps.runWizard(cmd, res, core, true)
	return err
}

// Messages of the first-run gate of "worker up".
const (
	setupFlagsHint = "Add --share-existing all (or none, or later) to the command; add --server <url> to use a Hub"
)

// serverHint is the part of a message that names --server, only when no Hub
// address or join token is given yet (A58: no advice that does not apply).
func serverHint(res *config.Resolver) string {
	if hub, err := config.ResolveWorkerHub(res); err == nil && hub.Server != "" {
		return ""
	}
	if r, err := res.Resolve(config.KeyJoin); err == nil && r.Value != "" {
		return ""
	}
	return " (and --server <url> to use a Hub)"
}

// firstRun is the gate of "worker up": when the Worker has never been set up
// it runs the wizard on a terminal, and otherwise it refuses with the exact
// way out. It never reads input and never starts a listener without a
// terminal. A Worker counts as set up when the sharing choice is given by a
// flag, the environment or the config file, or was saved before (by this
// wizard or by "worker config share").
//
// owner is the user whose Worker is set up when "worker up" runs as root for
// somebody else (nil otherwise): root asks on its terminal what the Hub may
// see of that user's sessions and saves the answer as that user (A57).
func (d workerSetupDeps) firstRun(cmd *cobra.Command, res *config.Resolver, st *upState, owner *ownerSetup) error {
	if d.assumeSetUp {
		return nil
	}
	core, err := config.ResolveWorkerCore(res)
	if err != nil {
		return err
	}
	if st == nil {
		st = &upState{dir: core.StateDir}
	}
	core.StateDir = st.dir
	ctx := cmdContext(cmd)
	if d.checkTerminal != nil {
		if err := d.checkTerminal(ctx, res, core.TmuxSocket, owner); err != nil {
			return err
		}
	}
	// Root reads configuration through the directory handle. The owner reads
	// their own database when the sharing choice is absent from configuration.
	needed, err := d.setupNeeded(ctx, res, core.StateDir, owner == nil)
	if err != nil {
		return err
	}
	if needed && owner != nil {
		// A previous interactive setup saves sharing in the target's database.
		// Read it under that UID, never by opening their SQLite files as root.
		if _, err := st.root.Lstat(store.DBFile); err == nil {
			r, err := readUpStatus(ctx, st.dir, owner)
			if err != nil {
				return err
			}
			shareUnread := r.Share.Mode == ""
			for _, pr := range r.Problems {
				if strings.Contains(pr, "share selection") {
					shareUnread = true
				}
			}
			if !r.Share.Configured && shareUnread {
				// The database exists but its choice could not be read (an
				// older schema, for example): never replace it with a default.
				return fmt.Errorf("the saved sharing choice of %s could not be read, so nothing was changed; run as %s: minion worker config share all (or none, or later), then this command again",
					owner.Target.User, owner.Target.User)
			}
			needed = !r.Share.Configured
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	if !needed {
		// The sharing choice is given (--share-existing, for example), so
		// the wizard does not run; a first pairing on a terminal still asks
		// for the Hub fingerprint before anything dials.
		return d.fingerprintGate(cmd, res, st)
	}
	interactive := d.isInteractive(cmd.InOrStdin(), cmd.OutOrStdout())
	fallback, _ := cmd.Flags().GetString("non-interactive-share")
	if !interactive && fallback != "" {
		switch fallback {
		case config.ShareExistingAll, config.ShareExistingNone, config.ShareExistingLater:
		default:
			return errors.New("--non-interactive-share requires all, none or later")
		}
		if owner != nil {
			return d.saveOwnerShare(ctx, owner, fallback, cmd.OutOrStdout())
		}
		db, err := d.open(ctx, st.dir)
		if err != nil {
			return err
		}
		defer db.Close()
		return db.SaveShareContext(ctx, store.ShareSelection{Mode: store.ShareMode(fallback)})
	}
	switch {
	case owner != nil && interactive:
		if err := d.askOwnerShare(cmd, owner); err != nil {
			return err
		}
		return d.fingerprintGate(cmd, res, st)
	case owner != nil:
		return fmt.Errorf("this Worker is not set up yet and there is no terminal to ask what the Hub may see of %s's tmux sessions, so nothing was started. "+
			"Add --share-existing all (or none, or later) to the command. "+
			"To run the Worker as root and serve root's tmux sessions instead, add --as-user root", owner.Target.User)
	case !interactive:
		return fmt.Errorf("this Worker is not set up yet and there is no terminal to ask what the Hub may see, so nothing was started. "+
			"Add --share-existing all (or none, or later) to the command%s, or run `minion worker setup` in a terminal first", serverHint(res))
	}
	_, err = d.runWizard(cmd, res, core, false)
	return err
}

// fingerprintGate asks for the expected Hub fingerprint (M1) when "worker up"
// on a terminal is about to pair for the first time without the wizard: a Hub
// address is set, no hub_fingerprint and no invitation key are given, and the
// Worker is not enrolled. It runs before any network probe. A fingerprint
// typed here is saved as hub_fingerprint in the config file, so the Worker
// that starts next pairs only with that Hub; "skip" pairs as before (the
// Worker warns that the Hub is not verified). Without a terminal nothing is
// asked and the Worker warns.
func (d workerSetupDeps) fingerprintGate(cmd *cobra.Command, res *config.Resolver, st *upState) error {
	hub, err := config.ResolveWorkerHub(res)
	if err != nil {
		return err
	}
	if hub.Server == "" || hub.Fingerprint != "" {
		return nil
	}
	if given, _, err := config.InviteKeyGiven(res); err != nil || given {
		return err
	}
	in, out := cmd.InOrStdin(), cmd.OutOrStdout()
	if !d.isInteractive(in, out) {
		return nil
	}
	ctx := cmdContext(cmd)
	if enrolled, err := st.enrolled(ctx); err != nil || enrolled {
		return err
	}
	m, err := agent.AskHubFingerprint(in, out, hub.Server)
	if err != nil {
		return fmt.Errorf("nothing was started: %w", err)
	}
	if m.IsZero() {
		return nil
	}
	path, err := st.saveHubFingerprint(m.String())
	if err != nil {
		return fmt.Errorf("cannot save %s, nothing was started: %w", config.KeyHubFingerprint, err)
	}
	_, _ = fmt.Fprintf(out, "Saved %s in %s; the Worker pairs only with that Hub.\n", config.KeyHubFingerprint, path)
	return nil
}

// enrolledReadOnly reports whether the Worker state in stateDir holds an
// enrollment. It never creates or migrates the database: a missing one is
// "not enrolled".
func enrolledReadOnly(ctx context.Context, stateDir string) (bool, error) {
	if _, err := os.Stat(filepath.Join(stateDir, store.DBFile)); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, fmt.Errorf("look at the Worker state in %s: %w", stateDir, err)
	}
	st, err := store.OpenReadOnlyContext(ctx, stateDir)
	if err != nil {
		return false, fmt.Errorf("open the Worker state in %s: %w", stateDir, err)
	}
	defer func() { _ = st.Close() }()
	ok, err := st.HasEnrollment(ctx)
	if err != nil && strings.Contains(err.Error(), "no such table") {
		return false, nil // a state of a release before pairing
	}
	return ok, err
}

// setupNeeded reports whether the Worker has no sharing choice yet.
func (d workerSetupDeps) setupNeeded(ctx context.Context, res *config.Resolver, stateDir string, readState bool) (bool, error) {
	share, err := res.Resolve(config.KeyShareExisting)
	if err != nil {
		return false, err
	}
	if share.Value != "" || !readState {
		return share.Value == "", nil
	}
	// A saved choice counts. The database is opened only when it exists, so a
	// check never creates a state.
	if _, err := os.Stat(filepath.Join(stateDir, store.DBFile)); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return true, nil
		}
		return false, fmt.Errorf("look at the Worker state in %s: %w", stateDir, err)
	}
	// Read-only first: a wizard that follows and is cancelled must leave the
	// state unmigrated. A state that cannot be read so is opened as before.
	if d.openStore == nil {
		if ro, err := store.OpenReadOnlyContext(ctx, stateDir); err == nil {
			sel, err := ro.LoadShareContext(ctx)
			_ = ro.Close()
			if err == nil {
				return !sel.Configured, nil
			}
		}
	}
	st, err := d.open(ctx, stateDir)
	if err != nil {
		return false, fmt.Errorf("open the Worker state in %s: %w", stateDir, err)
	}
	defer func() { _ = st.Close() }()
	sel, err := st.LoadShareContext(ctx)
	if err != nil {
		return false, err
	}
	return !sel.Configured, nil
}

func cmdContext(cmd *cobra.Command) context.Context {
	if ctx := cmd.Context(); ctx != nil {
		return ctx
	}
	return context.Background()
}

// runWizard runs the terminal wizard on the real state of the Worker.
//
// explicit is true for "minion worker setup": values from the config file and
// the saved state are offered as defaults, and only flags and environment
// variables answer questions. For the gate of "worker up" (explicit false) the
// file counts too, so that a Hub address saved earlier is not asked again.
func (d workerSetupDeps) runWizard(cmd *cobra.Command, res *config.Resolver, core config.WorkerCore, explicit bool) (setup.Result, error) {
	hub, err := config.ResolveWorkerHub(res)
	if err != nil {
		return setup.Result{}, err
	}
	provided, err := providedFrom(res, !explicit)
	if err != nil {
		return setup.Result{}, err
	}
	in, out := cmd.InOrStdin(), cmd.OutOrStdout()
	interactive := d.isInteractive(in, out)
	if !interactive && provided.Share == "" {
		return setup.Result{}, fmt.Errorf("there is no terminal to ask in. %s. Or run `minion worker setup` in a terminal", setupFlagsHint)
	}

	ctx, stop := signal.NotifyContext(cmdContext(cmd), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// The state is opened for writing (and migrated) only when the wizard
	// saves or pairs: a wizard cancelled before changes nothing on disk.
	st := &lazySetupStore{dir: core.StateDir, open: d.open}
	defer func() { _ = st.Close() }()

	checker, err := d.hubChecker(hub)
	if err != nil {
		return setup.Result{}, err
	}
	var lister setup.SessionLister
	if d.sessions != nil {
		lister = d.sessions(core.TmuxSocket)
	} else if s := tmuxSessions(core.TmuxSocket); s != nil {
		lister = s
	}
	dialer := d.dialer
	if dialer == nil {
		dialer = agent.NewPairDialer()
	}
	w := setup.New(setup.Config{
		Sessions: lister,
		Persist:  &setupPersister{stateDir: core.StateDir, st: st},
		Check:    checker,
		Pairing: &setupPairing{
			enr: agent.NewEnrollments(st, core.StateDir), name: core.Name, dialer: dialer,
			clock: d.clock, backoff: d.backoff, newCode: d.newCode,
		},
		Provided:          provided,
		AskHubFingerprint: true,
	})
	t := &setup.Terminal{In: in, Out: out, Interactive: interactive}
	skip, err := inviteSkipMessage(res, explicit)
	if err != nil {
		return setup.Result{}, err
	}
	t.SkipPairing = skip
	if interactive {
		_, _ = fmt.Fprintln(out, "Minion Worker setup")
	}
	result, err := t.Run(ctx, w)
	switch {
	case errors.Is(err, setup.ErrNeedsTerminal):
		return result, fmt.Errorf("there is no terminal to ask in. %s", setupFlagsHint)
	case errors.Is(err, context.Canceled) && result.Summary.Share.Mode != "":
		// Stopped while waiting for the pairing, after the settings were saved:
		// "setup" is finished (the dialog said so); "worker up" stops here.
		if explicit {
			return result, nil
		}
	}
	return result, err
}

// inviteSkipMessage returns the text that replaces the pairing step when an
// invitation key is configured, and "" when none is. It uses
// [config.InviteKeyGiven], the detector of the enrollment hook itself, so the
// gate agrees with the hook on every source: the flag (an empty flag means "no
// key" and hides lower sources), MINION_INVITE_KEY_FILE and the config file.
// It only looks: it never reads the key, a file or standard input, because
// that could take what the wizard needs for its questions.
//
// For the gate of "worker up" (explicit false) the enrollment hook runs right
// after the wizard, with the same settings, so the text promises it. "minion
// worker setup" (explicit true) runs no hook, enrolls nothing and keeps no
// key: a key given by flag or environment is not carried over to the next
// command, and the text says so; only a key already stored in the config file
// is still there for "worker up".
func inviteSkipMessage(res *config.Resolver, explicit bool) (string, error) {
	given, _, err := config.InviteKeyGiven(res)
	if err != nil {
		return "", err
	}
	if !given {
		return "", nil
	}
	if !explicit {
		return "An invitation key was given. This Worker enrolls with it when it starts (minion worker up), so no pairing code is needed.", nil
	}
	transient := *res
	transient.File = nil
	if viaFlagOrEnv, _, err := config.InviteKeyGiven(&transient); err != nil {
		return "", err
	} else if !viaFlagOrEnv {
		return "An invitation key is stored in the config file. This setup does not use it; `minion worker up` enrolls this " +
			"Worker with it when it starts, so no pairing code is needed.", nil
	}
	return "An invitation key was given. This setup does not use it, does not enroll and does not keep it: give it again to " +
		"`minion worker up` (--invite-key <file>, or MINION_INVITE_KEY_FILE), which enrolls this Worker at the Hub when it " +
		"starts. No pairing code is needed.", nil
}

// providedFrom collects the answers that flags and environment variables (and,
// with withFile, the config file) already give.
func providedFrom(res *config.Resolver, withFile bool) (setup.Provided, error) {
	var p setup.Provided
	counts := func(r config.Resolved) bool {
		return r.Source == config.SourceFlag || r.Source == config.SourceEnv || (withFile && r.Source == config.SourceFile)
	}
	share, err := res.Resolve(config.KeyShareExisting)
	if err != nil {
		return p, err
	}
	if share.Value != "" && counts(share) {
		p.Share = store.ShareMode(share.Value)
	}
	server, err := res.Resolve(config.KeyServer)
	if err != nil {
		return p, err
	}
	if counts(server) {
		// An empty value is an explicit Solo only from a flag: an empty
		// variable counts as unset.
		if server.Value != "" || server.Source == config.SourceFlag {
			url, err := setup.ValidateHubAddress(server.Value)
			if err != nil {
				return p, fmt.Errorf("setting %q (%s): %w", config.KeyServer, server.Source, err)
			}
			p.Server, p.HasServer = url, true
		}
	}
	fp, err := res.Resolve(config.KeyHubFingerprint)
	if err != nil {
		return p, err
	}
	if v := fp.Value; v != "" {
		m, err := agent.ParseHubExpectation(v)
		if err != nil {
			return p, err
		}
		p.HubFingerprint = m.String()
	}
	return p, nil
}

// hubChecker builds the Hub address check from the Hub settings: hub_ca_file
// adds the CA that makes a certificate trusted and hub_insecure switches the
// trust check off, exactly as they do for the other plain HTTPS calls. The
// Worker's own connection ignores both (A-15).
func (d workerSetupDeps) hubChecker(hub config.WorkerHub) (setup.HubChecker, error) {
	if d.checker != nil {
		return d.checker(hub)
	}
	c := &setup.Checker{Insecure: hub.Insecure}
	if hub.CAFile != "" {
		pem, err := readSmallFile(hub.CAFile, 1<<20)
		if err != nil {
			return nil, fmt.Errorf("read %s (hub_ca_file): %w", config.KeyHubCAFile, err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("hub_ca_file %q holds no PEM certificate", hub.CAFile)
		}
		c.RootCAs = pool
	}
	return c, nil
}

func readSmallFile(path string, limit int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	b, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, fmt.Errorf("the file is larger than %d bytes", limit)
	}
	return b, nil
}

// setupPersister saves the wizard's choices into the config file and the
// Worker state database, under the same lock as "worker config".
type setupPersister struct {
	stateDir string
	st       shareStore
}

func (p *setupPersister) configPath() string { return filepath.Join(p.stateDir, workerConfigName) }

func (p *setupPersister) Current(ctx context.Context) (setup.Saved, error) {
	sel, err := p.st.LoadShareContext(ctx)
	if err != nil {
		return setup.Saved{}, err
	}
	file, err := config.LoadFile(p.configPath())
	if err != nil {
		return setup.Saved{}, err
	}
	server, _, _ := file.Lookup(config.KeyServer)
	return setup.Saved{Share: sel, Server: server}, nil
}

// Save writes the config file first and the database second. The database is
// the one that can refuse (a locked or full disk); when it does, the file is
// put back, so the call changes nothing. A crash between the two leaves the
// file at the new values, and a share_existing in it is applied to the
// database at the next start, so a start never reinstates the old choice
// (the same order as "worker config share").
func (p *setupPersister) Save(ctx context.Context, s setup.Settings) error {
	if err := os.MkdirAll(p.stateDir, 0o700); err != nil {
		return fmt.Errorf("cannot create the state directory %q: %w", p.stateDir, err)
	}
	// The wait for the lock ends with ctx (Ctrl-C, SIGTERM), and ctx is checked
	// again before each of the two writes, so that a cancelled wizard never
	// persists a new Hub address, a share choice, or a half of them.
	release, err := lockConfigFileContext(ctx, filepath.Join(p.stateDir, configLockName))
	if err != nil {
		return err
	}
	defer release()
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("setup was cancelled; nothing was changed: %w", err)
	}
	file, err := config.LoadFile(p.configPath())
	if err != nil {
		return err
	}
	prevShare, hadShare, _ := file.Lookup(config.KeyShareExisting)
	prevServer, hadServer, _ := file.Lookup(config.KeyServer)
	prevFP, hadFP, _ := file.Lookup(config.KeyHubFingerprint)

	if hadShare {
		// share_existing has no "select": that mode removes it and the saved
		// selection stands.
		if s.Share.Mode == store.ShareSelect {
			file.Delete(config.KeyShareExisting)
		} else {
			file.Set(config.KeyShareExisting, string(s.Share.Mode))
		}
	}
	if s.Server == "" {
		file.Delete(config.KeyServer)
	} else {
		file.Set(config.KeyServer, s.Server)
	}
	// The expected Hub fingerprint is saved in the same write as the
	// address, so the Worker service applies it when it pairs by itself.
	if s.HubFingerprint != "" {
		file.Set(config.KeyHubFingerprint, s.HubFingerprint)
	}
	if hadShare || hadServer || s.Server != "" {
		if err := file.Save(); err != nil {
			return fmt.Errorf("the config file %s could not be updated, nothing was changed: %w", file.Path(), err)
		}
	}
	err = ctx.Err()
	if err == nil {
		err = p.st.SaveShareContext(ctx, s.Share)
	}
	if err != nil {
		if hadShare {
			file.Set(config.KeyShareExisting, prevShare)
		}
		if hadServer {
			file.Set(config.KeyServer, prevServer)
		} else {
			file.Delete(config.KeyServer)
		}
		if hadFP {
			file.Set(config.KeyHubFingerprint, prevFP)
		} else {
			file.Delete(config.KeyHubFingerprint)
		}
		if hadShare || hadServer || s.Server != "" {
			if rerr := file.Save(); rerr != nil {
				return fmt.Errorf("%w; the config file %s could not be put back (%v)", err, file.Path(), rerr)
			}
		}
		return err
	}
	return nil
}

// setupPairing runs the existing pairing flow for the wizard.
type setupPairing struct {
	enr     *agent.Enrollments
	name    string
	dialer  agent.PairDialer
	clock   transport.Clock
	backoff *transport.Backoff
	newCode func() identity.PairingCode
}

func (p *setupPairing) Paired(ctx context.Context) (setup.Paired, bool, error) {
	e, ok, err := p.enr.Load(ctx)
	if err != nil || !ok {
		return setup.Paired{}, false, err
	}
	return setup.Paired{Owner: e.Owner, Hub: e.HubURL}, true, nil
}

// Pair uses the Worker's pairer as "minion worker pair" does: it shows the
// code on out and waits for the Hub. The pairer's log is discarded here
// because the human sees the code on out; the code is printed once.
func (p *setupPairing) Pair(ctx context.Context, hubURL, fingerprint string, out io.Writer) (setup.Paired, error) {
	expect, err := agent.ParseHubExpectation(fingerprint)
	if err != nil {
		return setup.Paired{}, err
	}
	pr, err := agent.NewPairer(agent.PairerConfig{
		ExpectHub:  expect,
		HubURL:     hubURL,
		Name:       p.name,
		Enrollment: p.enr,
		Dialer:     p.dialer,
		Clock:      p.clock,
		Backoff:    p.backoff,
		NewCode:    p.newCode,
		Out:        out,
	})
	if err != nil {
		return setup.Paired{}, err
	}
	enr, err := pr.Run(ctx)
	if err != nil {
		return setup.Paired{}, err
	}
	return setup.Paired{Owner: enr.Owner}, nil
}
