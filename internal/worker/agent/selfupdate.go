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
	// updateCommitAfter is how long a new binary must run with a live Hub
	// connection before the update counts as good and the guard is cleared.
	updateCommitAfter = 5 * time.Minute
	// guardWindow and guardMaxStarts: a new binary started this many times
	// within the window without committing is rolled back.
	guardWindow    = 10 * time.Minute
	guardMaxStarts = 3
)

// pendingFile is the crash-guard record in the state directory.
const pendingFile = "update-pending.json"

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
	res := ops.WorkerUpdateResult{From: version.Version, To: args.Version}
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
	go u.run(context.WithoutCancel(ctx), args.Version, hub)
	res.Accepted = true
	return res, nil
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
func (u *selfUpdate) run(ctx context.Context, tag string, hub *version.ProtoRange) {
	ctx, cancel := context.WithTimeout(ctx, selfUpdateTimeout)
	defer cancel()
	fail := func(err error) {
		_ = os.Remove(filepath.Join(u.stateDir, pendingFile))
		u.log.Warn("worker update failed", "to", tag, "error", err.Error())
		u.setState(ops.UpdateStateFailed, version.Version, tag, err.Error())
		u.release()
	}
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
	if err := writePending(u.stateDir, pendingRecord{From: version.Version, To: tag, At: u.now()}); err != nil {
		fail(fmt.Errorf("cannot write the update guard, so no rollback would be possible: %w", err))
		return
	}
	r, err := inst.Run(ctx, version.Options{Selector: sel, Current: version.Version, HubRange: hub, ExecPath: u.exe})
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
	time.Sleep(restartDelay)
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
	b, err := os.ReadFile(filepath.Join(dir, pendingFile))
	if err != nil {
		return pendingRecord{}, false
	}
	var r pendingRecord
	if json.Unmarshal(b, &r) != nil {
		return pendingRecord{}, false
	}
	return r, true
}

// guardOnStart runs once when the Worker starts, before the modules. For a
// new binary whose update is not committed yet it records the start; on the
// guardMaxStarts-th start within guardWindow it restores "<exe>.old" and asks
// for a restart. When the previous binary runs with a guard that names the
// newer target, the update was rolled back: it returns that record once.
func guardOnStart(log *slog.Logger, dir, exe, running string, now time.Time) (rolledBack *pendingRecord, restart bool) {
	r, ok := readPending(dir)
	if !ok {
		return nil, false
	}
	switch running {
	case r.From:
		_ = os.Remove(filepath.Join(dir, pendingFile))
		return &r, false
	case r.To:
	default:
		_ = os.Remove(filepath.Join(dir, pendingFile))
		return nil, false
	}
	// Each recorded start that did not commit is a failed start. Roll back
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
		return nil, false
	}
	old := exe + ".old"
	if _, err := os.Stat(old); err != nil {
		log.Error("the updated Worker keeps failing to start and there is no previous binary to restore", "to", r.To, "error", err.Error())
		_ = os.Remove(filepath.Join(dir, pendingFile))
		return nil, false
	}
	// The database first: a migration of the new release may have changed it,
	// and the old binary refuses a newer schema. Without a restored database
	// the rollback stops here and the guard stays, so the next start retries.
	if err := restoreStateDB(dir, r.At); err != nil {
		log.Error("cannot restore the Worker database from its pre-migration backup; the rollback waits for the next start", "error", err.Error())
		_ = writePending(dir, r)
		return nil, false
	}
	if err := restoreOld(exe, old); err != nil {
		log.Error("cannot restore the previous Worker binary", "error", err.Error())
		_ = writePending(dir, r)
		return nil, false
	}
	_ = writePending(dir, r) // the restored binary finds From == running and reports the rollback
	log.Warn("the updated Worker failed to start; restored the previous binary", "from", r.To, "to", r.From)
	return nil, true
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

// commitAfter clears the guard once the new binary ran with a live Hub
// connection for updateCommitAfter.
func commitAfter(ctx context.Context, dir, running string, connected func() bool, tick <-chan time.Time) {
	var since time.Time
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-tick:
			if !connected() {
				since = time.Time{}
				continue
			}
			if since.IsZero() {
				since = now
			}
			if now.Sub(since) >= updateCommitAfter {
				if r, ok := readPending(dir); ok && r.To == running {
					_ = os.Remove(filepath.Join(dir, pendingFile))
				}
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
// is rolled back here. It restarts the process when it restored the previous
// binary, and returns the rolled-back record to report otherwise.
func RecoverUpdate(log *slog.Logger, stateDir string) *pendingRecord {
	if stateDir == "" {
		return nil
	}
	exe, err := os.Executable()
	if err != nil {
		return nil
	}
	if real, err := filepath.EvalSymlinks(exe); err == nil {
		exe = real
	}
	rolled, restart := guardOnStart(log, stateDir, exe, version.Version, time.Now())
	if restart {
		restartProcess(log, exe)
	}
	return rolled
}
