package agent

// Remote self-update of the Worker (v0.9.0, A10): the Hub asks for one exact
// signed release (worker_update); the Worker answers at once, installs it in
// the background with the same verified updater as "minion update" and
// restarts. A crash guard restores the previous binary when the new one does
// not come up. The design follows proven updaters: Tailscale's control push
// with a local veto and an install-method check, Syncthing's exit-code
// restart and syscall.Exec, Teleport's rollback of a version that fails.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/patrick-1984/ai-minion/internal/identity"
	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
	"github.com/patrick-1984/ai-minion/internal/version"
	"github.com/patrick-1984/ai-minion/internal/worker/store"
)

// Timings of the self-update.
const (
	// selfUpdateTimeout bounds one download, verification and install.
	selfUpdateTimeout = 20 * time.Minute
	// restartDelay lets the answer and the log reach the Hub before the
	// process restarts.
	restartDelay = 2 * time.Second
	// replyBound is the longest the restart waits for the answer to be
	// written; restartGrace follows a written answer.
	replyBound   = 10 * time.Second
	restartGrace = 300 * time.Millisecond
	// earlyFailWait is how long worker_update waits for a failure that shows
	// at once (the release is not published, the lookup fails) before it
	// answers "accepted". A slower failure is read with worker_update_status.
	earlyFailWait = 5 * time.Second
	// updateCommitAfter is how long a new binary must run with a live Hub
	// connection before the update counts as good and the guard is cleared.
	updateCommitAfter = 5 * time.Minute
	// guardWindow and guardMaxStarts: a new binary started this many times
	// within the window without committing and without a clean stop is
	// rolled back (boot counting as in systemd-boot and Android A/B updates:
	// only a start that neither reached the "good" mark nor ended cleanly is
	// a failed start).
	guardWindow    = 10 * time.Minute
	guardMaxStarts = 3
)

// pendingFile is the crash-guard record in the state directory.
const pendingFile = "update-pending.json"

// pendingMu serialises every write and removal of the crash-guard record
// inside one process: the installer of a new update (selfUpdate.run), the
// commit (connector module) and the clean-stop record (Runtime) can run at
// the same time, and each changes only the trial it owns. Other processes are
// kept out by the state's lifecycle lock.
var pendingMu sync.Mutex

// updateEnrollmentFile names the trial during which a Hub enrollment changed.
// A rollback of that trial could restore the earlier enrollment from backup.
const updateEnrollmentFile = "update-enrollment"

// exitRestart is the exit code that asks the service manager (systemd
// Restart=always, launchd KeepAlive, the Windows service recovery) to start
// the Worker again. 75 is EX_TEMPFAIL.
const exitRestart = 75

// releaseInstaller installs one release; *version.Updater implements it.
type releaseInstaller interface {
	Run(ctx context.Context, o version.Options) (version.Result, error)
}

// selfUpdate is the Worker side of worker_update.
type selfUpdate struct {
	log      *slog.Logger
	stateDir string
	exe      string
	// block is why the Worker cannot update itself; empty when it can.
	block string
	// installer builds the verified updater for one run.
	installer func() (releaseInstaller, error)
	// hubRange is the protocol range of the connected Hub (nil: unknown).
	hubRange func() *version.ProtoRange
	// sessionsLost reports how many sessions a restart would end (ConPTY);
	// nil means none (tmux sessions survive the Worker).
	sessionsLost func(ctx context.Context) int
	// restart ends the process so that it starts again with the new binary.
	restart func()
	// workerID is this Worker's Hub id (Policy.WorkerID); a request must name it.
	workerID func() string
	// holdSessions stops (true) or allows again (false) the creation of new
	// sessions, so that none appears between the session check and the
	// restart (Executor.SetSessionCreateHold).
	holdSessions func(reason string)
	now          func() time.Time
	// earlyWait overrides earlyFailWait (tests); zero means earlyFailWait.
	earlyWait time.Duration
	// apk is set on the Android app: the update installs a new APK through
	// the app instead of replacing the binary (selfupdate_apk.go).
	apk *apkUpdater

	busy atomic.Bool
	mu   sync.Mutex
	st   ops.WorkerUpdateStatus
}

// register adds worker_update and worker_update_status to the executor.
func (u *selfUpdate) register(e *Executor) {
	e.Handle(ops.OpWorkerUpdate, u.handleUpdate)
	e.Handle(ops.OpWorkerUpdateStatus, u.handleStatus)
}

func (u *selfUpdate) setState(state, from, to, msg string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	at := ops.Timestamp(u.now())
	u.st = ops.WorkerUpdateStatus{State: state, Running: version.Version, From: from, To: to,
		Error: proto.SafeText(msg, 500), At: &at}
}

func (u *selfUpdate) status() ops.WorkerUpdateStatus {
	u.mu.Lock()
	defer u.mu.Unlock()
	st := u.st
	if st.State == "" {
		st.State = ops.UpdateStateIdle
	}
	st.Running = version.Version
	return st
}

func (u *selfUpdate) handleStatus(_ context.Context, rq Request) (any, error) {
	_, args, err := prepare[ops.WorkerUpdateStatusArgs](rq)
	if err != nil {
		return nil, err
	}
	if err := u.fromHub(rq, args.WorkerID); err != nil {
		return nil, err
	}
	return u.status(), nil
}

// fromHub admits only a request the Hub routed to this Worker: the local
// socket and local MCP pass every action, so they must not reach the update.
func (u *selfUpdate) fromHub(rq Request, workerID string) error {
	if rq.Origin == nil {
		return apiErr(proto.CodeForbidden, "", "the Worker update is run by an administrator through the Hub; use minion update locally")
	}
	if u.workerID == nil || workerID == "" || workerID != u.workerID() {
		return apiErr(proto.CodeForbidden, "", "worker_id names another Worker")
	}
	return nil
}

// handleUpdate checks the request and starts the update in the background.
// Every "no" is an answer with a reason, not an error: the Hub shows it.
func (u *selfUpdate) handleUpdate(ctx context.Context, rq Request) (any, error) {
	_, args, err := prepare[ops.WorkerUpdateArgs](rq)
	if err != nil {
		return nil, err
	}
	if err := u.fromHub(rq, args.WorkerID); err != nil {
		return nil, err
	}
	if u.apk != nil {
		return u.handleAPKUpdate(ctx, args)
	}
	res := ops.WorkerUpdateResult{From: version.Version, To: args.Version}
	u.log.Debug("worker update requested", "from", version.Version, "to", args.Version, "force", args.Force, "block", u.block)
	defer func() {
		if res.Reason != "" {
			u.log.Debug("worker update refused", "from", version.Version, "to", args.Version, "reason", res.Reason)
		}
	}()
	target, err := version.ParseVersion(args.Version)
	if err != nil || args.Version != target.String() {
		return nil, apiErr(proto.CodeInvalidArgument, "version", "version must be one exact release such as v0.9.0 (no channel)")
	}
	if u.block != "" {
		res.Reason = u.block
		return res, nil
	}
	cur, err := version.ParseVersion(version.Version)
	if err != nil {
		res.Reason = "this Worker is a development build; update it by hand"
		return res, nil
	}
	if c := target.Compare(cur); c <= 0 {
		if c == 0 {
			res.Reason = "already on " + version.Version
		} else {
			res.Reason = "refusing a downgrade from " + version.Version
		}
		return res, nil
	}
	// The protocol range of the Hub that asks, taken now: the job must never
	// install a release this Hub cannot talk to, even if it disconnects.
	var hub *version.ProtoRange
	if u.hubRange != nil {
		hub = u.hubRange()
	}
	if hub == nil {
		res.Reason = "not connected to a Hub"
		return res, nil
	}
	if !u.busy.CompareAndSwap(false, true) {
		res.Reason = "an update is already running"
		return res, nil
	}
	// No new session from here on; then count the ones a restart would end.
	if u.holdSessions != nil {
		u.holdSessions("the Worker is updating")
	}
	if !args.Force && u.sessionsLost != nil {
		if n := u.sessionsLost(ctx); n > 0 {
			u.release()
			res.Reason = fmt.Sprintf("%d sessions would end on the restart (ConPTY); update with force to accept that", n)
			return res, nil
		}
	}
	u.setState(ops.UpdateStateDownloading, version.Version, args.Version, "")
	failed := make(chan string, 1)
	go u.run(context.WithoutCancel(ctx), args.Version, hub, failed)
	wait := u.earlyWait
	if wait == 0 {
		wait = earlyFailWait
	}
	select {
	case msg := <-failed:
		if msg != "" { // "" means: installed, the restart follows the answer
			res.Reason = msg
			return res, nil
		}
	case <-time.After(wait):
	case <-ctx.Done():
	}
	res.Accepted = true
	return res, nil
}

// blockReason is why the Worker cannot update itself now, "" when it can: the
// fixed reason of the install, then (Android) the app's readiness.
func (u *selfUpdate) blockReason() string {
	if u.block != "" {
		return u.block
	}
	if u.apk != nil {
		return u.apk.block()
	}
	return ""
}

// release ends an update that did not restart: sessions may be created again.
func (u *selfUpdate) release() {
	if u.holdSessions != nil {
		u.holdSessions("")
	}
	u.busy.Store(false)
}

// run installs the release and restarts. The crash guard is written before
// anything is replaced; without it nothing is installed.
func (u *selfUpdate) run(ctx context.Context, tag string, hub *version.ProtoRange, failed chan<- string) {
	ctx, cancel := context.WithTimeout(ctx, selfUpdateTimeout)
	defer cancel()
	// The guard of this run, and the one it replaces (the trial of the
	// running, still uncommitted binary), which a failure puts back.
	var mine, prev pendingRecord
	var wrote, hadPrev bool
	fail := func(err error) {
		if wrote {
			pendingMu.Lock()
			if cur, ok := readPending(u.stateDir); ok && cur.sameTrial(mine) {
				if hadPrev {
					_ = writePending(u.stateDir, prev)
				} else {
					_ = os.Remove(filepath.Join(u.stateDir, pendingFile))
				}
			}
			pendingMu.Unlock()
		}
		// The error can quote a release mirror URL (update_api_url) with a
		// credential; neither the log nor the reported state repeats it.
		msg := errString(err)
		u.log.Warn("worker update failed", "to", tag, "error", msg)
		u.setState(ops.UpdateStateFailed, version.Version, tag, msg)
		u.release()
		if failed != nil {
			select {
			case failed <- proto.SafeText(msg, 500):
			default:
			}
		}
	}
	u.log.Debug("worker update: preparing the installer", "to", tag, "exe", u.exe)
	inst, err := u.installer()
	if err != nil {
		fail(err)
		return
	}
	sel, err := parseExactSelector(tag)
	if err != nil {
		fail(err)
		return
	}
	mine = pendingRecord{From: version.Version, To: tag, At: u.now()}
	pendingMu.Lock()
	prev, hadPrev = readPending(u.stateDir)
	err = writePending(u.stateDir, mine)
	pendingMu.Unlock()
	if err != nil {
		fail(fmt.Errorf("cannot write the update guard, so no rollback would be possible: %w", err))
		return
	}
	wrote = true
	u.log.Debug("worker update: crash guard written; downloading, verifying and installing", "to", tag,
		"hub_proto_min", hub.Min.String(), "hub_proto_max", hub.Max.String())
	r, err := inst.Run(ctx, version.Options{Selector: sel, Current: version.Version, HubRange: hub, ExecPath: u.exe})
	u.log.Debug("worker update: installer finished", "to", tag, "installed", r.Tag, "up_to_date", r.UpToDate, "error", errString(err))
	if err != nil {
		fail(err)
		return
	}
	if r.UpToDate {
		fail(errors.New("already on " + r.Tag))
		return
	}
	u.setState(ops.UpdateStateRestarting, version.Version, r.Tag, "")
	u.log.Info("worker updated; restarting", "from", version.Version, "to", r.Tag)
	if failed != nil { // wake the handler: the answer goes out before the restart
		select {
		case failed <- "":
		default:
		}
	}
	// The answer goes out first: wait for its write (bounded, so a stalled
	// link cannot hold the update), then keep the short grace for the log.
	if sent := ReplySent(ctx); sent != nil {
		select {
		case <-sent:
			time.Sleep(restartGrace)
		case <-time.After(replyBound):
		}
	} else {
		time.Sleep(restartDelay)
	}
	u.restart()
}

// parseExactSelector is the selector of one exact version.
func parseExactSelector(tag string) (version.Selector, error) {
	sel, err := version.ParseSelector(tag)
	if err != nil {
		return version.Selector{}, err
	}
	if sel.IsChannel() {
		return version.Selector{}, errors.New("not an exact version: " + tag)
	}
	return sel, nil
}

// pendingRecord is the crash guard of an installed, not yet committed update.
type pendingRecord struct {
	From   string    `json:"from"`
	To     string    `json:"to"`
	At     time.Time `json:"at"`
	Starts []int64   `json:"starts,omitempty"` // unix seconds of the starts of the new binary
}

// sameTrial reports whether r and o guard the same update attempt.
func (r pendingRecord) sameTrial(o pendingRecord) bool {
	return r.From == o.From && r.To == o.To && r.At.Equal(o.At)
}

func writePending(dir string, r pendingRecord) error {
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	tmp := filepath.Join(dir, pendingFile+".tmp")
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(dir, pendingFile))
}

func readPending(dir string) (pendingRecord, bool) {
	r, err := loadPending(dir)
	return r, err == nil
}

func loadPending(dir string) (pendingRecord, error) {
	b, err := os.ReadFile(filepath.Join(dir, pendingFile))
	if err != nil {
		return pendingRecord{}, err
	}
	var r pendingRecord
	err = json.Unmarshal(b, &r)
	return r, err
}

// protectEnrollmentFromRollback records the pending trial before a new
// enrollment can replace the old one or clear its forget record. The marker
// belongs to this trial only, so later updates can still roll back.
func protectEnrollmentFromRollback(dir string) error {
	if dir == "" {
		return nil
	}
	r, err := loadPending(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read the pending update before recording the Hub enrollment: %w", err)
	}
	if err := identity.WritePrivate(filepath.Join(dir, updateEnrollmentFile), []byte(r.At.UTC().Format(time.RFC3339Nano))); err != nil {
		return fmt.Errorf("record the enrollment for the update guard: %w", err)
	}
	return nil
}

func rollbackTrustHold(ctx context.Context, dir string, r pendingRecord) (bool, error) {
	if _, err := os.Lstat(filepath.Join(dir, ForgottenHubFile)); !errors.Is(err, os.ErrNotExist) {
		return err == nil, err
	}
	raw, err := os.ReadFile(filepath.Join(dir, updateEnrollmentFile))
	if err == nil {
		at, err := time.Parse(time.RFC3339Nano, string(raw))
		if err != nil || at.Equal(r.At) {
			return err == nil, err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return false, err
	}

	if _, err := os.Lstat(filepath.Join(dir, store.DBFile)); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	st, err := store.OpenReadOnlyContext(ctx, dir)
	if err != nil {
		return false, err
	}
	defer func() { _ = st.Close() }()
	return st.HubQuarantined(ctx)
}

// guardOnStart runs once when the Worker starts, before the modules. For a
// new binary whose update is not committed yet it records the start and
// returns it (started), so that a clean stop of this run can take it back
// (cleanStop); on the guardMaxStarts-th failed start within guardWindow it
// restores "<exe>.old" and asks for a restart. When the previous binary runs
// with a guard that names the newer target, the update was rolled back: it
// returns that record once.
func guardOnStart(log *slog.Logger, dir, exe, running string, now time.Time) (rolledBack *pendingRecord, restart bool, started int64) {
	r, ok := readPending(dir)
	if !ok {
		return nil, false, 0
	}
	switch running {
	case r.From:
		_ = os.Remove(filepath.Join(dir, pendingFile))
		return &r, false, 0
	case r.To:
	default:
		_ = os.Remove(filepath.Join(dir, pendingFile))
		return nil, false, 0
	}
	// Each recorded start is one that neither committed nor stopped cleanly
	// (a clean stop removes its start, cleanStop): a failed start. Roll back
	// when guardMaxStarts of them happened within the window; otherwise record
	// this start and let it try.
	var failed []int64
	for _, s := range r.Starts {
		if now.Sub(time.Unix(s, 0)) < guardWindow {
			failed = append(failed, s)
		}
	}
	if len(failed) < guardMaxStarts {
		r.Starts = append(failed, now.Unix())
		_ = writePending(dir, r)
		return nil, false, now.Unix()
	}
	why := fmt.Sprintf("%d starts within %s that neither stopped cleanly nor stayed connected to the Hub for %s",
		len(failed), guardWindow, updateCommitAfter)
	// Pairing can write from another process, so keep the enrollment locked
	// from the trust check through any restore.
	ctx := context.Background()
	unlock, err := NewEnrollments(nil, dir).lockState(ctx)
	if err != nil {
		log.Error("cannot lock the Hub enrollment; rollback waits for the next start", "error", err)
		return nil, false, 0
	}
	defer unlock()
	hold, err := rollbackTrustHold(ctx, dir, r)
	if err != nil {
		log.Error("cannot read the Hub trust state; rollback waits for the next start", "error", err)
		return nil, false, 0
	}
	if hold {
		log.Warn("the updated Worker keeps failing to start; rollback would undo a Hub trust decision", "to", r.To, "reason", why)
		_ = os.Remove(filepath.Join(dir, pendingFile))
		return nil, false, 0
	}
	old := exe + ".old"
	if _, err := os.Stat(old); err != nil {
		log.Error("the updated Worker keeps failing to start and there is no previous binary to restore", "to", r.To, "reason", why, "error", err.Error())
		_ = os.Remove(filepath.Join(dir, pendingFile))
		return nil, false, 0
	}
	// The database first: a migration of the new release may have changed it,
	// and the old binary refuses a newer schema. Without a restored database
	// the rollback stops here and the guard stays, so the next start retries.
	if err := restoreStateDB(dir, r.At); err != nil {
		log.Error("cannot restore the Worker database from its pre-migration backup; the rollback waits for the next start", "error", err.Error())
		_ = writePending(dir, r)
		return nil, false, 0
	}
	if err := restoreOld(exe, old); err != nil {
		log.Error("cannot restore the previous Worker binary", "error", err.Error())
		_ = writePending(dir, r)
		return nil, false, 0
	}
	_ = writePending(dir, r) // the restored binary finds From == running and reports the rollback
	log.Warn("the updated Worker failed to start ("+why+"); restored the previous binary", "from", r.To, "to", r.From)
	return nil, true, 0
}

// cleanStop takes the start recorded at started (guardOnStart) back from the
// crash guard: the run of the new binary ended cleanly (Ctrl+C, "minion worker
// down", a service stop), so it was not a failed start. Manual restarts of an
// update that waits for its Hub (Hub down, removed by the Hub) then never roll
// it back. It does nothing once the update is committed or for another update.
func cleanStop(dir, running string, started int64) {
	if dir == "" || started == 0 {
		return
	}
	pendingMu.Lock()
	defer pendingMu.Unlock()
	r, ok := readPending(dir)
	if !ok || r.To != running {
		return
	}
	for i := len(r.Starts) - 1; i >= 0; i-- {
		if r.Starts[i] == started {
			r.Starts = append(r.Starts[:i:i], r.Starts[i+1:]...)
			_ = writePending(dir, r)
			return
		}
	}
}

// restoreOld puts "<exe>.old" back in place of exe.
func restoreOld(exe, old string) error {
	failed := exe + ".failed"
	_ = os.Remove(failed)
	if err := os.Rename(exe, failed); err != nil {
		return err
	}
	if err := os.Rename(old, exe); err != nil {
		_ = os.Rename(failed, exe)
		return err
	}
	return nil
}

// updateHealth is what the crash guard learns from the connector on a tick.
type updateHealth int

const (
	// healthNone: neither connected nor deliberately stopped.
	healthNone updateHealth = iota
	// healthConnected: a live Hub connection.
	healthConnected
	// healthTrustHold: the Worker deliberately does not connect because the
	// operator quarantined its Hub (verify-hub) or removed the pairing
	// (--forget), or because it is not enrolled (the Hub removed it, or it
	// waits to be paired). That is a state of a working release, not a crash:
	// the update is committed at once, because a rollback would restore the
	// pre-migration database and the previous binary, which know neither the
	// quarantine nor the removal and would connect to the disputed Hub again,
	// or would lose a pairing made meanwhile.
	healthTrustHold
)

// commitAfter clears the guard once the new binary ran with a live Hub
// connection for updateCommitAfter, or at once when the Worker holds a trust
// decision (healthTrustHold).
func commitAfter(ctx context.Context, dir, running string, health func() updateHealth, tick <-chan time.Time) {
	commit := func() {
		pendingMu.Lock()
		defer pendingMu.Unlock()
		if r, ok := readPending(dir); ok && r.To == running {
			_ = os.Remove(filepath.Join(dir, pendingFile))
		}
	}
	var since time.Time
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-tick:
			switch health() {
			case healthTrustHold:
				commit()
				return
			case healthConnected:
			default:
				since = time.Time{}
				continue
			}
			if since.IsZero() {
				since = now
			}
			if now.Sub(since) >= updateCommitAfter {
				commit()
				return
			}
		}
	}
}

// restoreStateDB replaces the Worker database with the oldest pre-migration
// backup written after since (<state>/backups, see internal/worker/store), and
// removes the WAL and shared-memory files of the replaced database. Without
// such a backup the database was not migrated and stays.
func restoreStateDB(stateDir string, since time.Time) error {
	dir := filepath.Join(stateDir, "backups")
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil // no migration ran: the database is the old one
	}
	if err != nil {
		return err // unknown: never pair the old binary with a newer schema
	}
	var best string
	var bestAt time.Time
	for _, e := range entries {
		info, err := e.Info()
		if err != nil || !info.Mode().IsRegular() || filepath.Ext(e.Name()) != ".db" || info.ModTime().Before(since) {
			continue
		}
		if best == "" || info.ModTime().Before(bestAt) {
			best, bestAt = filepath.Join(dir, e.Name()), info.ModTime()
		}
	}
	if best == "" {
		return nil
	}
	db := filepath.Join(stateDir, store.DBFile)
	for _, sfx := range []string{"-wal", "-shm"} {
		_ = os.Remove(db + sfx)
	}
	b, err := os.ReadFile(best)
	if err != nil {
		return err
	}
	tmp := db + ".restore"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, db)
}

// RecoverUpdate runs the crash guard at the very start of the Worker, before
// any module, migration or setting can fail: a new release that cannot start
// is rolled back here. When it restored the previous binary it calls release
// (which must free everything a new Worker process needs, such as the state's
// lifecycle lock) and hands over to the restored binary (restartNow), never
// returning. When ctx ended meanwhile (Ctrl+C or a service stop during the
// restore), it does not start the restored binary and returns an error that
// wraps ctx.Err() instead: the caller stops through its normal path (a
// Windows service then reports SERVICE_STOPPED), and the restored binary,
// already in place, runs on the next start. Otherwise it returns the
// rolled-back record to report, and the start it recorded for an uncommitted
// update (0: none), which a clean stop gives back (cleanStop).
func RecoverUpdate(ctx context.Context, log *slog.Logger, stateDir string, release func()) (rolled *pendingRecord, started int64, err error) {
	if stateDir == "" {
		return nil, 0, nil
	}
	exe, err := os.Executable()
	if err != nil {
		return nil, 0, nil
	}
	if real, err := filepath.EvalSymlinks(exe); err == nil {
		exe = real
	}
	return recoverUpdate(ctx, log, stateDir, exe, time.Now(), release, restartNow)
}

func recoverUpdate(ctx context.Context, log *slog.Logger, stateDir, exe string, now time.Time, release func(),
	restart func(*slog.Logger, string)) (*pendingRecord, int64, error) {
	rolled, again, started := guardOnStart(log, stateDir, exe, version.Version, now)
	if again {
		// A rollback happens before any module runs: hand over to the
		// restored binary at once, never continue in the rejected one.
		if release != nil {
			release()
		}
		if err := ctx.Err(); err != nil {
			log.Info("stop requested during the rollback; the restored binary runs on the next start", "exe", exe)
			return nil, 0, fmt.Errorf("agent: stopped during the update rollback: %w", err)
		}
		restart(log, exe)
		// restart does not return in production; never run on in the
		// rejected binary if it does.
		return nil, 0, errors.New("agent: the restart with the restored binary returned")
	}
	return rolled, started, nil
}
