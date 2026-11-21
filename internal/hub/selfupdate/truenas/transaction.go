package truenas

import (
	"context"
	"errors"
	"slices"
	"time"
)

// errPending means the work cannot finish in this tick (a middleware job is
// still running, or a step failed in a way that the next tick may fix). The
// journal stays at its phase and the next tick resumes.
var errPending = errors.New("reconcile pending")

// phase records the intent of the next side effect (durable before it).
func (e *Executor) phase(a *Attempt, p string) error {
	a.Phase = p
	if err := e.Journal.SaveAttempt(a); err != nil {
		return fail("journal_unwritable", err)
	}
	return nil
}

// deadline helpers use the injected clock so tests do not wait.
func (e *Executor) poll(ctx context.Context, limit time.Duration, code string, done func() (bool, error)) error {
	deadline := e.now().Add(limit)
	for {
		ok, err := done()
		if err != nil {
			return err
		}
		if ok {
			return nil
		}
		if !e.now().Before(deadline) {
			return fail(code, nil)
		}
		if err := e.sleep(ctx, e.Timeouts.Poll); err != nil {
			return fail("canceled", err)
		}
	}
}

func (e *Executor) waitJob(ctx context.Context, id int, limit time.Duration) error {
	return e.poll(ctx, limit, "job_timeout", func() (bool, error) {
		s, err := e.MW.JobState(ctx, id)
		if err != nil {
			return false, fail("middleware_unavailable", err)
		}
		switch {
		case s.Active():
			return false, nil
		case s == JobSuccess:
			return true, nil
		default:
			return false, fail("job_failed", nil)
		}
	})
}

// runJob submits a middleware job, records its id in the journal and waits.
func (e *Executor) runJob(ctx context.Context, a *Attempt, op string, submit func() (int, error)) error {
	id, err := submit()
	if err != nil {
		return fail("middleware_"+op+"_failed", err)
	}
	a.Jobs = append(a.Jobs, JobRef{Op: op, ID: id})
	if err := e.Journal.SaveAttempt(a); err != nil {
		return fail("journal_unwritable", err)
	}
	if err := e.waitJob(ctx, id, e.Timeouts.Job); err != nil {
		return err
	}
	a.Jobs[len(a.Jobs)-1].Done = true
	return e.Journal.SaveAttempt(a)
}

// waitNoContainers returns when no container of the app runs.
func (e *Executor) waitNoContainers(ctx context.Context) error {
	return e.poll(ctx, e.Timeouts.Stop, "stop_timeout", func() (bool, error) {
		cs, err := e.Docker.Containers(ctx, e.project())
		if err != nil {
			return false, fail("docker_unavailable", err)
		}
		for _, c := range cs {
			if c.Running {
				return false, nil
			}
		}
		return true, nil
	})
}

func (e *Executor) stopApp(ctx context.Context, a *Attempt) error {
	st, err := e.MW.AppState(ctx, e.Cfg.App)
	if err != nil {
		return fail("middleware_unavailable", err)
	}
	if st != AppStopped {
		if err := e.runJob(ctx, a, "stop", func() (int, error) { return e.MW.Stop(ctx, e.Cfg.App) }); err != nil {
			return err
		}
	}
	return e.waitNoContainers(ctx)
}

// verify waits for sustained readiness of the container that runs the wanted
// image id: the container of the service must run exactly that image, and
// /readyz must answer for Sustain without interruption, within Ready.
func (e *Executor) verify(ctx context.Context, wantImageID string) error {
	var okSince time.Time
	return e.poll(ctx, e.Timeouts.Ready, "not_ready", func() (bool, error) {
		cs, err := e.Docker.Containers(ctx, e.project())
		good := false
		if err == nil {
			if c := runningOf(cs, e.Cfg.Service); c != nil && c.ImageID == wantImageID {
				pctx, cancel := context.WithTimeout(ctx, 10*time.Second)
				good = e.Probe.Ready(pctx) == nil
				cancel()
			}
		}
		if !good {
			okSince = time.Time{}
			return false, nil
		}
		now := e.now()
		if okSince.IsZero() {
			okSince = now
		}
		return now.Sub(okSince) >= e.Timeouts.Sustain, nil
	})
}

// transact is the forward path. Any error returned leaves the journal at the
// phase that failed; the caller then restores.
func (e *Executor) transact(ctx context.Context, a *Attempt, after Compose) error {
	// 1. Stop the app and prove that no writer is left.
	if err := e.phase(a, PhaseStopping); err != nil {
		return err
	}
	if err := e.stopApp(ctx, a); err != nil {
		return err
	}

	// 2. Snapshot exactly the Hub dataset while nothing runs, and hold it.
	if err := e.phase(a, PhaseSnapshotting); err != nil {
		return err
	}
	zctx, cancel := context.WithTimeout(ctx, e.Timeouts.ZFS)
	err := e.ZFS.Snapshot(zctx, a.Dataset, a.Snapshot)
	cancel()
	if err != nil {
		return fail("snapshot_failed", err)
	}
	zctx, cancel = context.WithTimeout(ctx, e.Timeouts.ZFS)
	err = e.ZFS.Hold(zctx, a.Dataset, a.Snapshot)
	cancel()
	if err != nil {
		return fail("snapshot_hold_failed", err)
	}

	// 3. Stage the candidate compose while stopped (app.update does not
	// start a stopped app). The stored document must still be the one the
	// attempt started from: this run needs exclusive control of the app.
	cur, err := e.MW.AppConfig(ctx, a.App)
	if err != nil {
		return fail("middleware_unavailable", err)
	}
	if cur.Hash() != a.ComposeBeforeHash {
		return fail("compose_drift", nil)
	}
	if err := e.phase(a, PhaseUpdating); err != nil {
		return err
	}
	if err := e.runJob(ctx, a, "update", func() (int, error) { return e.MW.Update(ctx, a.App, after) }); err != nil {
		return err
	}
	staged, err := e.MW.AppConfig(ctx, a.App)
	if err != nil {
		return fail("middleware_unavailable", err)
	}
	if img, _ := staged.Image(a.Service); img != a.CandidateRef {
		return fail("stored_pin_mismatch", nil)
	}

	// 4. Start the candidate. From here the data may be migrated.
	a.CandidateStarted = true
	if err := e.phase(a, PhaseStarting); err != nil {
		return err
	}
	if err := e.runJob(ctx, a, "start", func() (int, error) { return e.MW.Start(ctx, a.App) }); err != nil {
		return err
	}

	// 5. Sustained readiness of the candidate image id, then commit.
	if err := e.phase(a, PhaseVerifying); err != nil {
		return err
	}
	if err := e.verify(ctx, a.CandidateImageID); err != nil {
		return err
	}
	return e.commit(ctx, a)
}

func (e *Executor) commit(ctx context.Context, a *Attempt) error {
	st, err := e.Journal.LoadState()
	if err != nil {
		return fail("journal_unreadable", err)
	}
	st.LastTag = a.ToTag
	if err := e.Journal.SaveState(st); err != nil {
		return fail("journal_unwritable", err)
	}
	if err := e.phase(a, PhaseCommitted); err != nil {
		return err
	}
	e.Journal.DropCompose()
	e.dropSnapshot(ctx, a)
	return nil
}

// dropSnapshot releases and destroys the attempt's snapshot. It is
// idempotent and best effort: a leftover snapshot is harmless, and the next
// tick sweeps it (see sweep). It reports whether nothing is left.
func (e *Executor) dropSnapshot(ctx context.Context, a *Attempt) bool {
	if a.Snapshot == "" {
		return true
	}
	zctx, cancel := context.WithTimeout(ctx, e.Timeouts.ZFS)
	defer cancel()
	ok, err := e.ZFS.SnapshotExists(zctx, a.Dataset, a.Snapshot)
	if err != nil {
		return false
	}
	if ok {
		// Release may fail when the hold was never taken; destroy then decides.
		_ = e.ZFS.Release(zctx, a.Dataset, a.Snapshot)
		if err := e.ZFS.Destroy(zctx, a.Dataset, a.Snapshot); err != nil {
			e.logger().Warn("snapshot not destroyed", "attempt", a.ID)
			return false
		}
	}
	return true
}

// sweep finishes the cleanup of an attempt that ended (or was killed right
// after it ended). An attempt the operator resolved keeps its checkpoint.
func (e *Executor) sweep(ctx context.Context, a *Attempt) {
	switch a.Phase {
	case PhaseCommitted, PhaseRolledBack:
	case PhaseAborted:
		if a.Reason == "resolved_by_operator" {
			return
		}
	default:
		return
	}
	if a.Snapshot == "" {
		return
	}
	if e.dropSnapshot(ctx, a) {
		a.Snapshot = ""
		_ = e.Journal.SaveAttempt(a)
	}
	e.Journal.DropCompose()
}

var restoreOrder = []string{PhaseRestoreStop, PhaseRestoreRollback, PhaseRestoreUpdate, PhaseRestoreStart, PhaseRestoreVerify}

// restore returns the app to the previous release: stop, zfs rollback, saved
// compose back, start, verify the previous image. Every step converges from
// whatever state a killed run left, so the same function resumes at the
// recorded phase. It returns nil when the attempt reached a terminal phase
// (rolled_back or recovery_required) and errPending when the next tick should
// continue.
func (e *Executor) restore(ctx context.Context, a *Attempt, reason string) error {
	ctx = context.WithoutCancel(ctx) // the restore must not die with the run's deadline
	if !restorePhase(a.Phase) {
		a.Reason = reason
		if err := e.phase(a, PhaseRestoreStop); err != nil {
			return err
		}
	}
	idx := slices.Index(restoreOrder, a.Phase)
	needRecovery := func(code string) error {
		a.Reason = code
		return e.phase(a, PhaseRecoveryRequired)
	}
	pending := func(err error) error {
		e.logger().Warn("restore step failed; the next run retries", "attempt", a.ID, "code", codeOf(err))
		return errPending
	}

	if idx <= 0 {
		if err := e.phase(a, PhaseRestoreStop); err != nil {
			return err
		}
		if err := e.stopApp(ctx, a); err != nil {
			return pending(err)
		}
	}
	if idx <= 1 {
		if err := e.phase(a, PhaseRestoreRollback); err != nil {
			return err
		}
		if a.CandidateStarted {
			zctx, cancel := context.WithTimeout(ctx, e.Timeouts.ZFS)
			exists, err := e.ZFS.SnapshotExists(zctx, a.Dataset, a.Snapshot)
			cancel()
			if err != nil {
				return pending(fail("zfs_unavailable", err))
			}
			if !exists {
				return needRecovery("snapshot_missing")
			}
			zctx, cancel = context.WithTimeout(ctx, e.Timeouts.ZFS)
			err = e.ZFS.Rollback(zctx, a.Dataset, a.Snapshot)
			cancel()
			if err != nil {
				// Typically a newer snapshot exists: keep the held checkpoint.
				return needRecovery("rollback_failed")
			}
		}
	}
	if idx <= 2 {
		if err := e.phase(a, PhaseRestoreUpdate); err != nil {
			return err
		}
		before, err := e.Journal.LoadCompose()
		if err != nil {
			return needRecovery("saved_compose_missing")
		}
		cur, err := e.MW.AppConfig(ctx, a.App)
		if err != nil {
			return pending(fail("middleware_unavailable", err))
		}
		if cur.Hash() != before.Hash() {
			if err := e.runJob(ctx, a, "restore_update", func() (int, error) { return e.MW.Update(ctx, a.App, before) }); err != nil {
				return pending(err)
			}
		}
	}
	if idx <= 3 {
		if err := e.phase(a, PhaseRestoreStart); err != nil {
			return err
		}
		if err := e.waitStable(ctx, e.Timeouts.Job); err != nil {
			return pending(err)
		}
		st, err := e.MW.AppState(ctx, a.App)
		if err != nil {
			return pending(fail("middleware_unavailable", err))
		}
		if st != AppRunning {
			if err := e.runJob(ctx, a, "restore_start", func() (int, error) { return e.MW.Start(ctx, a.App) }); err != nil {
				return pending(err)
			}
		}
	}
	if err := e.phase(a, PhaseRestoreVerify); err != nil {
		return err
	}
	if err := e.verify(ctx, a.PreviousImageID); err != nil {
		return needRecovery("previous_not_ready")
	}
	st, err := e.Journal.LoadState()
	if err != nil {
		return fail("journal_unreadable", err)
	}
	if a.CandidateStarted && a.Reason != "interrupted" && !slices.Contains(st.Quarantine, a.CandidateDigest) {
		st.Quarantine = append(st.Quarantine, a.CandidateDigest)
		if err := e.Journal.SaveState(st); err != nil {
			return fail("journal_unwritable", err)
		}
	}
	if err := e.phase(a, PhaseRolledBack); err != nil {
		return err
	}
	e.Journal.DropCompose()
	e.dropSnapshot(ctx, a)
	return nil
}

// waitStable waits while the app is DEPLOYING.
func (e *Executor) waitStable(ctx context.Context, limit time.Duration) error {
	return e.poll(ctx, limit, "app_busy", func() (bool, error) {
		s, err := e.MW.AppState(ctx, e.Cfg.App)
		if err != nil {
			return false, fail("middleware_unavailable", err)
		}
		return s != AppDeploying, nil
	})
}

// reconcile finishes or undoes an unfinished attempt of an earlier run.
func (e *Executor) reconcile(ctx context.Context, a *Attempt) error {
	// 0. Never race a middleware job of the interrupted run.
	for i := range a.Jobs {
		if a.Jobs[i].Done {
			continue
		}
		s, err := e.MW.JobState(ctx, a.Jobs[i].ID)
		if err != nil {
			return errPending
		}
		if s.Active() {
			if err := e.waitJob(ctx, a.Jobs[i].ID, e.Timeouts.Reconcil); err != nil && codeOf(err) == "job_timeout" {
				return errPending
			}
		}
		a.Jobs[i].Done = true
	}
	if err := e.Journal.SaveAttempt(a); err != nil {
		return err
	}
	if err := e.waitStable(ctx, e.Timeouts.Reconcil); err != nil {
		return errPending
	}

	switch a.Phase {
	case PhasePrepared:
		a.Reason = "interrupted_before_start"
		if err := e.phase(a, PhaseAborted); err != nil {
			return err
		}
		e.Journal.DropCompose()
		return nil
	case PhaseStopping:
		if e.untouched(ctx, a) {
			a.Reason = "interrupted_before_stop"
			if err := e.phase(a, PhaseAborted); err != nil {
				return err
			}
			e.Journal.DropCompose()
			return nil
		}
	case PhaseStarting, PhaseVerifying:
		// The candidate may be fine: a run killed after a good start only
		// lost its commit. Prove it again before committing.
		if e.candidateRunning(ctx, a) {
			if err := e.phase(a, PhaseVerifying); err != nil {
				return err
			}
			verr := e.verify(ctx, a.CandidateImageID)
			if verr == nil {
				return e.commit(ctx, a)
			}
			a.Reason = codeOf(verr)
		}
	}
	reason := a.Reason
	if !restorePhase(a.Phase) && reason == "" {
		reason = "interrupted"
	}
	err := e.restore(ctx, a, reason)
	if errors.Is(err, errPending) {
		return errPending
	}
	return err
}

// untouched: the app still runs the previous release with the stored compose
// of the attempt's start, so nothing needs undoing.
func (e *Executor) untouched(ctx context.Context, a *Attempt) bool {
	st, err := e.MW.AppState(ctx, a.App)
	if err != nil || st != AppRunning {
		return false
	}
	cur, err := e.MW.AppConfig(ctx, a.App)
	if err != nil || cur.Hash() != a.ComposeBeforeHash {
		return false
	}
	cs, err := e.Docker.Containers(ctx, e.project())
	if err != nil {
		return false
	}
	c := runningOf(cs, a.Service)
	return c != nil && c.ImageID == a.PreviousImageID
}

func (e *Executor) candidateRunning(ctx context.Context, a *Attempt) bool {
	st, err := e.MW.AppState(ctx, a.App)
	if err != nil || st != AppRunning {
		return false
	}
	cs, err := e.Docker.Containers(ctx, e.project())
	if err != nil {
		return false
	}
	c := runningOf(cs, a.Service)
	return c != nil && c.ImageID == a.CandidateImageID
}

// ClearQuarantine forgets the quarantined digests (operator action).
func (e *Executor) ClearQuarantine() error {
	unlock, err := e.Journal.Lock()
	if err != nil {
		return err
	}
	defer unlock()
	st, err := e.Journal.LoadState()
	if err != nil {
		return err
	}
	st.Quarantine = nil
	return e.Journal.SaveState(st)
}

// ResolveRecovery closes an attempt in recovery_required after the operator
// restored the app by hand (see the recovery steps in the install guide).
func (e *Executor) ResolveRecovery() error {
	unlock, err := e.Journal.Lock()
	if err != nil {
		return err
	}
	defer unlock()
	a, err := e.Journal.LoadAttempt()
	if err != nil {
		return err
	}
	if a == nil || a.Phase != PhaseRecoveryRequired {
		return errors.New("no attempt needs recovery")
	}
	a.Reason = "resolved_by_operator"
	a.Phase = PhaseAborted
	if err := e.Journal.SaveAttempt(a); err != nil {
		return err
	}
	e.Journal.DropCompose()
	return nil
}
