package cli

// The staged join of "minion worker join --token-file PATH|-" and
// "minion worker join --resume" (tray tickets v3, C10 and W7a).
//
// A join has three stages, and the result names each one:
//
//	enrollment  committed | already           (failed when it stopped there)
//	config      saved | failed                 server and hub_fingerprint in the config file
//	activation  restarted | not_running | failed
//
// Exit 0 means all three are done. An enrollment that already exists is not
// a success by itself: the stages after it still have to be done, and
// --resume does them without the token, after it checked that the stored
// enrollment was authenticated by the Hub the config names. A stage that was
// not reached is left out of the result.
//
// The token is a secret: it is read from a file or stdin, never from argv,
// and never printed, logged or put into an error.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/patrick-1984/ai-minion/internal/client"
	"github.com/patrick-1984/ai-minion/internal/config"
	"github.com/patrick-1984/ai-minion/internal/identity"
	"github.com/patrick-1984/ai-minion/internal/worker/agent"
	"github.com/patrick-1984/ai-minion/internal/worker/store"
)

// Stage values of the join result.
const (
	joinCommitted  = "committed"
	joinAlready    = "already"
	joinFailed     = "failed"
	joinSaved      = "saved"
	joinRestarted  = "restarted"
	joinNotRunning = "not_running"
)

// maxJoinTokenInput bounds what is read from the token file or stdin.
const maxJoinTokenInput = maxSecretInput

// joinResult is the JSON result of the staged join.
type joinResult struct {
	Enrollment   string `json:"enrollment,omitempty"`
	Config       string `json:"config,omitempty"`
	Activation   string `json:"activation,omitempty"`
	StateDir     string `json:"state_dir,omitempty"`
	Hub          string `json:"hub,omitempty"`
	Instance     string `json:"instance,omitempty"`
	SessionsLost *int   `json:"sessions_lost,omitempty"`
}

func (r joinResult) summary() string {
	parts := []string{}
	for _, p := range [][2]string{{"enrollment", r.Enrollment}, {"config", r.Config}, {"activation", r.Activation}} {
		if p[1] != "" {
			parts = append(parts, p[0]+": "+p[1])
		}
	}
	return "Join: " + strings.Join(parts, ", ")
}

// runWorkerJoinStaged runs the staged join and ends it with the result
// contract (one JSON object with --json).
func runWorkerJoinStaged(cmd *cobra.Command, deps workerPairDeps, opt pairOptions, args []string) error {
	stdout, stderr := deps.stdout, deps.stderr
	if stdout == nil {
		stdout = cmd.OutOrStdout()
	}
	if stderr == nil {
		stderr = cmd.ErrOrStderr()
	}
	res, err := stagedJoin(cmd, deps, opt, args, stderr)
	if res.Enrollment != "" {
		human := stderr
		if !opt.asJSON && err == nil {
			human = stdout
		}
		_, _ = fmt.Fprintln(human, res.summary())
	}
	return emitResult(stdout, stderr, opt.asJSON, res, err)
}

func stagedJoin(cmd *cobra.Command, deps workerPairDeps, opt pairOptions, args []string, stderr io.Writer) (joinResult, error) {
	var res joinResult
	switch {
	case len(args) > 0 && identity.LooksLikeJoinToken(args[0]):
		// Never echo it: the token holds an invitation key.
		return res, usagef("a join token is never an argument: put it in a private file and give --token-file PATH, or pipe it with --token-file -")
	case len(args) > 0:
		return res, usagef("a Hub address and --token-file, --resume or --json do not go together; pair with a code by minion worker join <url>")
	case opt.tokenFile != "" && opt.resume:
		return res, usagef("give --token-file or --resume, not both")
	case opt.tokenFile == "" && !opt.resume:
		return res, usagef("give the join token with --token-file PATH|-, or finish an unfinished join with --resume")
	case opt.newCode:
		return res, usagef("--new-code goes with pairing by code, not with a join token")
	case opt.acceptLoss < 0:
		return res, usagef("--accept-loss takes a number of sessions, 0 or more")
	}

	r := &config.Resolver{Role: config.ScopeWorker, Flags: changedFlags{cmd: cmd, reg: config.Default}, LookupEnv: deps.lookupEnv}
	if deps.loadConfig != nil {
		file, err := loadConfigFile(r, deps.loadConfig)
		if err != nil {
			return res, err
		}
		r.File = file
	}
	core, err := config.ResolveWorkerCore(r)
	if err != nil {
		return res, err
	}
	res.StateDir = core.StateDir
	// A paused Worker talks to no Hub, and joining is Hub traffic (tray W3c).
	if p, err := agent.ReadPause(core.StateDir); err != nil {
		return res, err
	} else if p.Active(time.Now()) {
		return res, newResultError(ExitPaused, "this Worker is paused, so it talks to no Hub; resume it first (minion worker resume), then run this again. Nothing was changed.")
	}
	ctx, stop := signal.NotifyContext(cmdContext(cmd), os.Interrupt, syscall.SIGTERM)
	defer stop()

	share, err := r.Resolve(config.KeyShareExisting)
	if err != nil {
		return res, &usageError{err}
	}
	if opt.resume {
		if share.Source == config.SourceFlag {
			if err := commitJoinShare(ctx, deps, core, share.Value); err != nil {
				return res, err
			}
		}
		return resumeJoin(ctx, cmd, deps, opt, r, core, res)
	}

	// The first join needs the sharing choice; it is asked before the token
	// is read, so a refused run reads no secret.
	needed, err := workerSetupDeps{}.setupNeeded(ctx, r, core.StateDir, true)
	if err != nil {
		return res, err
	}
	if needed {
		return res, usagef("this Worker has no sharing choice yet: give --share-existing all|none|later (what the Hub may see of the tmux sessions). Nothing was changed")
	}

	tok, err := readJoinToken(ctx, opt.tokenFile, deps.stdin)
	if err != nil {
		return res, err
	}
	res.Hub = safeURL(tok.HubURL)
	// The token must name the Hub that flags, environment and the config
	// file name, if they name one; nothing is sent otherwise.
	const from = "the join token (--token-file)"
	explicit := &config.Resolver{Role: config.ScopeWorker, Flags: changedFlags{cmd: cmd, reg: config.Default}, LookupEnv: deps.lookupEnv}
	if err := joinServerConflict(explicit, from, tok); err != nil {
		return res, err
	}
	if err := joinFingerprintConflict(explicit, from, tok); err != nil {
		return res, err
	}
	if given, _, err := config.InviteKeyGiven(explicit); err != nil {
		return res, err
	} else if given {
		return res, errors.New("the join token holds a one-time invitation key, and --invite-key or MINION_INVITE_KEY_FILE gives another one; give only one of them")
	}
	if err := joinFileConflictWith(r.File, from, tok.HubURL, tok.Fingerprint().String()); err != nil {
		return res, err
	}
	hub, err := config.ResolveWorkerHub(r)
	if err != nil {
		return res, err
	}
	check := store.HubCheckInvite
	if strings.TrimSpace(hub.Fingerprint) != "" {
		check = store.HubCheckFingerprint
	}
	// The sharing choice is saved before the Worker enrolls: the Hub never
	// sees a session the choice would hide.
	if share.Source == config.SourceFlag {
		if err := commitJoinShare(ctx, deps, core, share.Value); err != nil {
			return res, err
		}
	}

	logger, err := commandLogger(cmd, r, stderr)
	if err != nil {
		return res, err
	}
	enrollment, err := enrollWithToken(ctx, deps, core, tok, check, logger)
	if err != nil {
		res.Enrollment = joinFailed
		return res, err
	}
	res.Enrollment = enrollment

	// Config: the address and the fingerprint of the token, unless the file
	// already holds a fingerprint (which matched the token above).
	fp := tok.Fingerprint().String()
	if v, ok, _ := fileValue(r.File, config.KeyHubFingerprint); ok && v != "" {
		fp = ""
	}
	if err := saveJoinConfig(deps, core.StateDir, tok.HubURL, fp); err != nil {
		res.Config = joinFailed
		return res, &resultError{Exit: ExitError, Message: "the Worker is enrolled, but the Hub address could not be saved in its config file (" +
			oneLine(err.Error()) + "); fix that, then run minion worker join --resume", Err: err}
	}
	res.Config = joinSaved
	return res, activateJoin(cmd, deps, opt, core.StateDir, &res, joinSavedText)
}

// readJoinToken reads and parses the join token from the file path, or from
// stdin for "-" (see [readSecretInput]). The token and the bytes read are
// never put into an error, and a file error does not name the path. A read
// that ctx ends is exit 130 (interrupted), with nothing changed.
func readJoinToken(ctx context.Context, path string, stdin io.Reader) (identity.JoinToken, error) {
	var none identity.JoinToken
	if identity.LooksLikeJoinToken(path) {
		return none, usagef("--token-file takes the path of a file that holds the join token, or - for stdin; the token itself is never an argument")
	}
	raw, err := readSecretInput(ctx, path, stdin, maxJoinTokenInput, "the join token")
	defer clear(raw)
	if errors.Is(err, errSecretInterrupted) {
		return none, newResultError(ExitInterrupted, "interrupted before the join token was read; nothing was changed")
	}
	if err != nil {
		return none, err
	}
	text := strings.TrimSpace(string(raw))
	if text == "" {
		return none, errors.New("malformed join token: it is empty; copy the whole token from the Hub's Add worker page")
	}
	tok, err := identity.ParseJoinToken(text)
	if err != nil {
		return none, fmt.Errorf("%w; copy the whole token from the Hub's Add worker page", err)
	}
	return tok, nil
}

// fileValue looks key up in the config file (nil: absent).
func fileValue(file config.FileSource, key string) (string, bool, error) {
	if file == nil {
		return "", false, nil
	}
	v, ok, err := file.Lookup(key)
	return strings.TrimSpace(v), ok, err
}

// enrollWithToken enrolls with the invitation key of tok under the
// enrollment lock, or finds the enrollment that already exists. An existing
// enrollment must be one with the token's Hub (exit 27 otherwise).
func enrollWithToken(ctx context.Context, deps workerPairDeps, core config.WorkerCore, tok identity.JoinToken, check string, logger *slog.Logger) (string, error) {
	release, err := agent.LockEnrollment(ctx, core.StateDir)
	if err != nil {
		return "", err
	}
	defer release()
	st, err := openJoinStore(ctx, deps, core.StateDir)
	if err != nil {
		return "", err
	}
	defer func() { _ = st.Close() }()
	enr := agent.NewEnrollments(st, core.StateDir)
	cur, ok, err := enr.Load(ctx)
	if err != nil {
		return "", fmt.Errorf("read the enrollment: %w", err)
	}
	if ok {
		m, err := agent.ParseHubExpectation(tok.Fingerprint().String())
		if err != nil {
			return "", err
		}
		if !agent.EnrollmentNamesHub(cur, m) || (cur.HubURL != "" && !sameHubURL(cur.HubURL, tok.HubURL)) {
			return "", newResultError(ExitStateChanged, "this Worker is already enrolled with another Hub than the one of the join token; "+
				"nothing was changed (leave that Hub first: minion worker reset)")
		}
		logger.Info("the Worker is already enrolled with the Hub of the join token; the invitation key is not used")
		return joinAlready, nil
	}
	if forgotten, err := agent.ForgottenHubs(core.StateDir); err != nil {
		return "", fmt.Errorf("read %s: %w", agent.ForgottenHubFile, err)
	} else if len(forgotten) > 0 {
		return "", errors.New(agent.ForgottenInviteMessage)
	}
	dialer := deps.inviteDialer
	if dialer == nil {
		dialer = agent.NewInviteDialer()
	}
	enroller, err := agent.NewInviteEnroller(agent.InviteEnrollerConfig{
		State: enr, Dialer: dialer, HubURL: tok.HubURL, Name: core.Name, HubCheck: check, Logger: logger,
	})
	if err != nil {
		return "", err
	}
	out, err := enroller.Enroll(ctx, tok.Invite)
	if err != nil {
		return "", err
	}
	if out.AlreadyEnrolled {
		return joinAlready, nil
	}
	return joinCommitted, nil
}

// openJoinStore opens worker.db without migrating it (exit 28 on another
// schema).
func openJoinStore(ctx context.Context, deps workerPairDeps, stateDir string) (*store.Store, error) {
	open := deps.openStore
	if open == nil {
		open = func(ctx context.Context, dir string) (*store.Store, error) {
			return store.OpenContext(ctx, dir, store.NoMigrate())
		}
	}
	st, err := open(ctx, stateDir)
	if errors.Is(err, store.ErrSchemaMismatch) {
		return nil, &resultError{Exit: ExitSchemaMismatch, Message: err.Error(), Err: err}
	}
	if err != nil {
		return nil, fmt.Errorf("open the Worker state in %s: %w", stateDir, err)
	}
	return st, nil
}

func saveJoinConfig(deps workerPairDeps, stateDir, url, fingerprint string) error {
	save := deps.saveServer
	if save == nil {
		save = saveWorkerHub
	}
	return save(stateDir, url, fingerprint)
}

// resumeJoin finishes an unfinished join without the token: it verifies the
// stored enrollment against the Hub the config (or a flag, or the
// environment) names, then saves the config and restarts the Worker. It never
// dials the Hub.
func resumeJoin(ctx context.Context, cmd *cobra.Command, deps workerPairDeps, opt pairOptions, r *config.Resolver, core config.WorkerCore, res joinResult) (joinResult, error) {
	cur, ok, err := readPairing(ctx, core.StateDir)
	if err != nil {
		return res, err
	}
	if !ok {
		return res, newResultError(ExitError, "this Worker is not enrolled, so there is no join to finish; join with minion worker join --token-file PATH|-")
	}
	hub, err := config.ResolveWorkerHub(r)
	if err != nil {
		return res, err
	}
	url := cur.HubURL
	switch {
	case hub.Server != "" && url != "" && !sameHubURL(hub.Server, url):
		return res, newResultError(ExitStateChanged, fmt.Sprintf("this Worker was enrolled by the Hub %s, but the configured Hub is %s; nothing was changed",
			safeURL(url), safeURL(hub.Server)))
	case url == "":
		url = hub.Server
	}
	if url == "" {
		return res, newResultError(ExitError, "the enrollment does not record its Hub address and none is configured; give it with --server")
	}
	res.Hub = safeURL(url)
	if strings.TrimSpace(hub.Fingerprint) != "" {
		m, err := agent.ParseHubExpectation(hub.Fingerprint)
		if err != nil {
			return res, err
		}
		if !agent.EnrollmentNamesHub(cur, m) {
			return res, newResultError(ExitStateChanged, "this Worker is enrolled with another Hub than the one hub_fingerprint names ("+m.Words()+"); nothing was changed")
		}
	}
	res.Enrollment = joinAlready

	fileServer, _, _ := fileValue(r.File, config.KeyServer)
	fileFp, _, _ := fileValue(r.File, config.KeyHubFingerprint)
	if fileServer == "" || !sameHubURL(fileServer, url) || fileFp == "" {
		fp := ""
		if fileFp == "" {
			f, err := identity.FingerprintOf(cur.Pin.Pub)
			if err != nil {
				return res, fmt.Errorf("read the pinned Hub key: %w", err)
			}
			fp = string(f)
		}
		if err := saveJoinConfig(deps, core.StateDir, url, fp); err != nil {
			res.Config = joinFailed
			return res, &resultError{Exit: ExitError, Message: "the Hub address could not be saved in the config file (" + oneLine(err.Error()) + ")", Err: err}
		}
	}
	res.Config = joinSaved
	return res, activateJoin(cmd, deps, opt, core.StateDir, &res, joinSavedText)
}

// joinSavedText says what a staged join did before its activation.
const joinSavedText = "the Worker is enrolled and its config is saved"

// activateJoin restarts the running Worker of stateDir so that it uses the
// new enrollment and config: activation restarted, not_running (exit 24) or
// failed (with the exit status of the restart, for example 23). saved says
// what was done before, for the messages.
func activateJoin(cmd *cobra.Command, deps workerPairDeps, opt pairOptions, stateDir string, res *joinResult, saved string) error {
	call := deps.call
	if call == nil {
		call = func(ctx context.Context, op string, args any) (json.RawMessage, error) {
			return client.Local().Call(ctx, op, args)
		}
	}
	live, err := liveReply(cmdContext(cmd), call)
	switch {
	case errors.Is(err, client.ErrWorkerNotRunning) || (err == nil && !sameDir(live.StateDir, stateDir)):
		res.Activation = joinNotRunning
		return newResultError(ExitNotRunning, saved+", but no Worker runs for "+stateDir+
			"; start it with minion worker start")
	case err != nil:
		res.Activation = joinFailed
		return fmt.Errorf("cannot ask the running Worker to restart: %w", err)
	}
	rd := defaultWorkerRestartDeps()
	rd.call = call
	if deps.restartSleep != nil {
		rd.sleep = deps.restartSleep
	}
	if deps.restartWait > 0 {
		rd.wait = deps.restartWait
	}
	rr, err := runWorkerRestart(cmd, rd, stateDir, opt.acceptLoss, opt.acceptUnknown)
	res.SessionsLost = rr.SessionsLost
	if err != nil {
		res.Activation = joinFailed
		var re *resultError
		if errors.As(err, &re) {
			wrapped := *re
			wrapped.Message = saved + ", but the Worker did not restart: " + re.Error() + resumeHint(saved)
			return &wrapped
		}
		return err
	}
	res.Activation = joinRestarted
	res.Instance = rr.Instance
	return nil
}

// commitJoinShare saves the sharing choice of --share-existing in the
// Worker's database (or through the running Worker), as "worker share set"
// does. A choice that the Worker's environment overrides at its next start
// is still saved: that is not a failure of the join.
func commitJoinShare(ctx context.Context, deps workerPairDeps, core config.WorkerCore, mode string) error {
	m := store.ShareMode(mode)
	if !m.Valid() || m == store.ShareSelect {
		return usagef("--share-existing takes all, none or later")
	}
	call := deps.call
	if call == nil {
		call = func(ctx context.Context, op string, args any) (json.RawMessage, error) {
			return client.Local().Call(ctx, op, args)
		}
	}
	open := deps.openStore
	if open == nil {
		open = func(ctx context.Context, dir string) (*store.Store, error) {
			return store.OpenContext(ctx, dir, store.NoMigrate())
		}
	}
	sd := workerShareDeps{lookupEnv: deps.lookupEnv, lockState: agent.LockLifecycle, lockConfig: lockConfigFile, openStore: open, call: call}
	_, err := shareWithCore(ctx, sd, core, &store.ShareChange{Mode: m})
	var re *resultError
	if errors.As(err, &re) && re.Exit == ExitNotInForce {
		return nil
	}
	if err != nil {
		return fmt.Errorf("save the sharing choice, nothing else was changed: %w", err)
	}
	return nil
}

// resumeHint is how to finish after a failed restart.
func resumeHint(saved string) string {
	if saved == joinSavedText {
		return " (finish with minion worker join --resume)"
	}
	return " (restart it with minion worker restart)"
}
