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
	now     func() time.Time

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
	if _, err := decode[ops.WorkerUpdateStatusArgs](rq.Args); err != nil {
		return nil, err
	}
	return u.status(), nil
}

// handleUpdate checks the request and starts the update in the background.
// Every "no" is an answer with a reason, not an error: the Hub shows it.
func (u *selfUpdate) handleUpdate(ctx context.Context, rq Request) (any, error) {
	args, err := decode[ops.WorkerUpdateArgs](rq.Args)
	if err != nil {
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
	if !args.Force && u.sessionsLost != nil {
		if n := u.sessionsLost(ctx); n > 0 {
			res.Reason = fmt.Sprintf("%d sessions would end on the restart (ConPTY); update with force to accept that", n)
			return res, nil
		}
	}
	if !u.busy.CompareAndSwap(false, true) {
		res.Reason = "an update is already running"
		return res, nil
	}
	u.setState(ops.UpdateStateDownloading, version.Version, args.Version, "")
	go u.run(context.WithoutCancel(ctx), args.Version, args.Force)
	res.Accepted = true
	return res, nil
}

// run installs the release and restarts.
func (u *selfUpdate) run(ctx context.Context, tag string, force bool) {
	defer u.busy.Store(false)
	ctx, cancel := context.WithTimeout(ctx, selfUpdateTimeout)
	defer cancel()
	fail := func(err error) {
		u.log.Warn("worker update failed", "to", tag, "error", err.Error())
		u.setState(ops.UpdateStateFailed, version.Version, tag, err.Error())
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
	var hub *version.ProtoRange
	if u.hubRange != nil {
		hub = u.hubRange()
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
	// Sessions may have appeared during the download (ConPTY): check again.
	if !force && u.sessionsLost != nil {
		if n := u.sessionsLost(ctx); n > 0 {
			fail(fmt.Errorf("%d sessions appeared during the download; %s is installed and starts with the next restart", n, r.Tag))
			return
		}
	}
	if err := writePending(u.stateDir, pendingRecord{From: version.Version, To: r.Tag, At: u.now()}); err != nil {
		u.log.Warn("cannot write the update guard; continuing without rollback", "error", err.Error())
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
	var recent []int64
	for _, s := range r.Starts {
		if now.Sub(time.Unix(s, 0)) < guardWindow {
			recent = append(recent, s)
		}
	}
	recent = append(recent, now.Unix())
	r.Starts = recent
	if len(recent) < guardMaxStarts {
		_ = writePending(dir, r)
		return nil, false
	}
	old := exe + ".old"
	if _, err := os.Stat(old); err != nil {
		log.Error("the updated Worker keeps failing to start and there is no previous binary to restore", "to", r.To, "error", err.Error())
		_ = os.Remove(filepath.Join(dir, pendingFile))
		return nil, false
	}
	if err := restoreOld(exe, old); err != nil {
		log.Error("cannot restore the previous Worker binary", "error", err.Error())
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
