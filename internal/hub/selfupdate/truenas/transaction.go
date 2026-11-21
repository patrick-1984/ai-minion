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

// poll runs done until it succeeds or the limit (measured on the injected
// clock) passes. Every call inside done carries its own deadline.
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
		s, err := e.jobState(ctx, id)
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

var methodOf = map[string]string{
	"stop": "app.stop", "start": "app.start", "update": "app.update",
	"restore_update": "app.update", "restore_start": "app.start", "finalize_update": "app.update",
}

// runJob records the intent (with what is needed to find the job again), then
// submits, records the job id and waits.
func (e *Executor) runJob(ctx context.Context, a *Attempt, op string, submit func(context.Context) (int, error)) error {
	a.Pending = &PendingJob{Op: op, Method: methodOf[op], Since: e.now().UTC()}
	if err := e.Journal.SaveAttempt(a); err != nil {
		return fail("journal_unwritable", err)
	}
	c, cancel := e.cc(ctx)
	id, err := submit(c)
	cancel()
	if err != nil {
		return fail("middleware_"+op+"_failed", err)
	}
	a.Jobs = append(a.Jobs, JobRef{Op: op, ID: id})
	a.Pending = nil
	if err := e.Journal.SaveAttempt(a); err != nil {
		return fail("journal_unwritable", err)
	}
	if err := e.waitJob(ctx, id, e.Timeouts.Job); err != nil {
		return err
	}
	a.Jobs[len(a.Jobs)-1].Done = true
	return e.Journal.SaveAttempt(a)
}

// settleJobs makes sure no middleware job of this attempt can still run:
// a job whose submission was not recorded is looked up, and an active job is
// waited for. It returns errPending while one may still run, so nothing is
// stopped, rolled back or started underneath it.
func (e *Executor) settleJobs(ctx context.Context, a *Attempt) error {
	if a.Pending != nil {
		c, cancel := e.cc(ctx)
		infos, err := e.MW.FindJobs(c, a.Pending.Method, a.App, a.Pending.Since)
		cancel()
		if err != nil {
			return errPending
		}
		for _, in := range infos {
			if !slices.ContainsFunc(a.Jobs, func(j JobRef) bool { return j.ID == in.ID }) {
				a.Jobs = append(a.Jobs, JobRef{Op: a.Pending.Op, ID: in.ID})
			}
		}
		a.Pending = nil
		if err := e.Journal.SaveAttempt(a); err != nil {
			return err
		}
	}
	for i := range a.Jobs {
		if a.Jobs[i].Done {
			continue
		}
		s, err := e.jobState(ctx, a.Jobs[i].ID)
		if err != nil {
			return errPending
		}
		if s.Active() {
			if err := e.waitJob(ctx, a.Jobs[i].ID, e.Timeouts.Reconcil); err != nil {
				if s2, err2 := e.jobState(ctx, a.Jobs[i].ID); err2 != nil || s2.Active() {
					return errPending
				}
			}
		}
		a.Jobs[i].Done = true
	}
	return e.Journal.SaveAttempt(a)
}

// waitNoContainers returns when no container of the app runs.
func (e *Executor) waitNoContainers(ctx context.Context) error {
	return e.poll(ctx, e.Timeouts.Stop, "stop_timeout", func() (bool, error) {
		cs, err := e.containers(ctx)
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
	st, err := e.appState(ctx)
	if err != nil {
		return fail("middleware_unavailable", err)
	}
	if st != AppStopped {
		if err := e.runJob(ctx, a, "stop", func(c context.Context) (int, error) { return e.MW.Stop(c, e.Cfg.App) }); err != nil {
			return err
		}
	}
	return e.waitNoContainers(ctx)
}

// verify waits for sustained readiness of the container that runs the wanted
// image id. Readiness is asked of that container's own address; the timer
// restarts whenever the container is replaced or restarted (id or start time
// changes) or its Docker health turns unhealthy.
func (e *Executor) verify(ctx context.Context, a *Attempt, wantImageID string, sustain time.Duration) error {
	var okSince time.Time
	ident := ""
	return e.poll(ctx, e.Timeouts.Ready, "not_ready", func() (bool, error) {
		cs, err := e.containers(ctx)
		good := false
		if err == nil {
			if c := runningOf(cs, e.Cfg.Service); c != nil && c.ImageID == wantImageID && c.Health != "unhealthy" {
				if id := c.ID + "@" + c.StartedAt; id != ident {
					ident, okSince = id, time.Time{}
				}
				good = e.readyNow(ctx, a, c) == nil
			} else {
				ident = ""
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
		return now.Sub(okSince) >= sustain, nil
	})
}

// docs rebuilds the three documents of an attempt from the saved one: the
// restore document (previous image by digest), the final one (candidate by
// digest) and the trial one (candidate, read-only Hub).
func (e *Executor) docs(a *Attempt) (restore, after, trial Compose, err error) {
	if restore, err = e.Journal.LoadCompose(a.SavedHash); err != nil {
		return
	}
	if after, err = restore.WithImage(a.Service, a.CandidateRef); err != nil {
		return
	}
	trial, err = after.WithMaintenance(a.Service)
	return
}

// foreign reports a stored document the updater cannot have written: it
// differs from the saved one in more than the image and the maintenance flag.
func foreign(cur, restore Compose, service string) bool {
	img, err := restore.Image(service)
	if err != nil {
		return true
	}
	n, err := cur.Normalized(service, img)
	if err != nil {
		return true
	}
	return n.Hash() != restore.Hash()
}

// checkDataset proves again that the configured dataset is still the one
// mounted at the Hub's data path.
func (e *Executor) checkDataset(ctx context.Context, a *Attempt) error {
	var info DatasetInfo
	err := e.zfsCall(ctx, func(c context.Context) (err error) { info, err = e.ZFS.DatasetAt(c, a.DataPath); return })
	if err != nil || info.Name != a.Dataset || info.GUID != a.DatasetGUID {
		return fail("dataset_changed", err)
	}
	return nil
}

// transact is the forward path. Any error returned leaves the journal at the
// phase that failed; the caller then restores.
func (e *Executor) transact(ctx context.Context, a *Attempt) error {
	_, after, trial, err := e.docs(a)
	if err != nil {
		return fail("saved_compose_missing", err)
	}

	// 1. Stop the app and prove that no writer is left.
	if err := e.phase(a, PhaseStopping); err != nil {
		return err
	}
	if err := e.stopApp(ctx, a); err != nil {
		return err
	}

	// 2. Snapshot exactly the Hub dataset while nothing runs, and hold it.
	if err := e.checkDataset(ctx, a); err != nil {
		return err
	}
	if err := e.phase(a, PhaseSnapshotting); err != nil {
		return err
	}
	if err := e.zfsCall(ctx, func(c context.Context) error { return e.ZFS.Snapshot(c, a.Dataset, a.Snapshot) }); err != nil {
		return fail("snapshot_failed", err)
	}
	if err := e.zfsCall(ctx, func(c context.Context) error { return e.ZFS.Hold(c, a.Dataset, a.Snapshot) }); err != nil {
		return fail("snapshot_hold_failed", err)
	}

	// 3. Stage the trial document while stopped (app.update does not start a
	// stopped app). The stored document must still be the attempt's own: an
	// administrator's edit aborts the attempt and is never overwritten.
	restore, _, _, _ := e.docs(a)
	cur, err := e.appConfig(ctx)
	if err != nil {
		return fail("middleware_unavailable", err)
	}
	if foreign(cur, restore, a.Service) {
		return fail("compose_drift", nil)
	}
	if err := e.phase(a, PhaseUpdating); err != nil {
		return err
	}
	if err := e.runJob(ctx, a, "update", func(c context.Context) (int, error) { return e.MW.Update(c, a.App, trial) }); err != nil {
		return err
	}
	if err := e.checkPin(ctx, a); err != nil {
		return err
	}

	// 4. Start the candidate read-only. From here the data may be migrated.
	a.CandidateStarted = true
	if err := e.phase(a, PhaseStarting); err != nil {
		return err
	}
	if err := e.runJob(ctx, a, "start", func(c context.Context) (int, error) { return e.MW.Start(c, a.App) }); err != nil {
		return err
	}

	// 5. Sustained readiness of the candidate while it rejects writes.
	if err := e.phase(a, PhaseVerifying); err != nil {
		return err
	}
	if err := e.verify(ctx, a, a.CandidateImageID, e.Timeouts.Sustain); err != nil {
		return err
	}
	return e.finalize(ctx, a, after)
}

func (e *Executor) checkPin(ctx context.Context, a *Attempt) error {
	staged, err := e.appConfig(ctx)
	if err != nil {
		return fail("middleware_unavailable", err)
	}
	if img, _ := staged.Image(a.Service); img != a.CandidateRef {
		return fail("stored_pin_mismatch", nil)
	}
	return nil
}

// finalize lifts the read-only flag (the middleware redeploys the running
// app with the final document), checks the candidate again and commits.
func (e *Executor) finalize(ctx context.Context, a *Attempt, after Compose) error {
	if err := e.phase(a, PhaseFinalizing); err != nil {
		return err
	}
	cur, err := e.appConfig(ctx)
	if err != nil {
		return fail("middleware_unavailable", err)
	}
	if cur.Env(a.Service, MaintenanceEnv) != "" { // not lifted yet (a killed run may have done it)
		if err := e.runJob(ctx, a, "finalize_update", func(c context.Context) (int, error) { return e.MW.Update(c, a.App, after) }); err != nil {
			return err
		}
	}
	if err := e.checkPin(ctx, a); err != nil {
		return err
	}
	if err := e.verify(ctx, a, a.CandidateImageID, e.Timeouts.Final); err != nil {
		return err
	}
	return e.commit(ctx, a)
}

// commit is one journal record: the attempt is committed and the installed
// tag follows from it (see syncLastTag).
func (e *Executor) commit(ctx context.Context, a *Attempt) error {
	if err := e.phase(a, PhaseCommitted); err != nil {
		return err
	}
	e.Journal.DropCompose()
	e.dropSnapshot(ctx, a)
	return nil
}

// dropSnapshot releases and destroys the attempt's snapshot. It is
// idempotent and best effort: a leftover snapshot is harmless, and the next
// tick sweeps it. It reports whether nothing is left.
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
// document back (previous image by digest), start, verify the previous image.
// Every step converges from whatever state a killed run left, so the same
// function resumes at the recorded phase. It returns nil when the attempt
// reached a terminal phase (rolled_back or recovery_required) and errPending
// when the next tick should continue. It has its own bounded budget and
// never runs while a middleware job of the attempt may still be active.
func (e *Executor) restore(ctx context.Context, a *Attempt, reason string) error {
	ctx = context.WithoutCancel(ctx)
	if e.Timeouts.Restore > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, e.Timeouts.Restore)
		defer cancel()
	}
	needRecovery := func(code string) error {
		a.Reason = code
		return e.phase(a, PhaseRecoveryRequired)
	}
	pending := func(err error) error {
		e.logger().Warn("restore step failed; the next run retries", "attempt", a.ID, "code", codeOf(err))
		return errPending
	}
	if err := e.settleJobs(ctx, a); err != nil {
		return err
	}
	restoreDoc, err := e.Journal.LoadCompose(a.SavedHash)
	if err != nil {
		return needRecovery("saved_compose_missing")
	}
	// An administrator's edit is never overwritten: classify before any change.
	cur, err := e.appConfig(ctx)
	if err != nil {
		return pending(fail("middleware_unavailable", err))
	}
	if foreign(cur, restoreDoc, a.Service) {
		return needRecovery("compose_changed_by_admin")
	}
	if !restorePhase(a.Phase) {
		a.Reason = reason
		if err := e.phase(a, PhaseRestoreStop); err != nil {
			return err
		}
	}
	idx := slices.Index(restoreOrder, a.Phase)

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
			if err := e.checkDataset(ctx, a); err != nil {
				return needRecovery("dataset_changed")
			}
			var exists bool
			if err := e.zfsCall(ctx, func(c context.Context) (err error) {
				exists, err = e.ZFS.SnapshotExists(c, a.Dataset, a.Snapshot)
				return
			}); err != nil {
				return pending(fail("zfs_unavailable", err))
			}
			if !exists {
				return needRecovery("snapshot_missing")
			}
			if err := e.zfsCall(ctx, func(c context.Context) error { return e.ZFS.Rollback(c, a.Dataset, a.Snapshot) }); err != nil {
				// Typically a newer snapshot exists: keep the held checkpoint.
				return needRecovery("rollback_failed")
			}
		}
	}
	if idx <= 2 {
		if err := e.phase(a, PhaseRestoreUpdate); err != nil {
			return err
		}
		cur, err := e.appConfig(ctx)
		if err != nil {
			return pending(fail("middleware_unavailable", err))
		}
		if cur.Hash() != restoreDoc.Hash() {
			if err := e.runJob(ctx, a, "restore_update", func(c context.Context) (int, error) { return e.MW.Update(c, a.App, restoreDoc) }); err != nil {
				return pending(err)
			}
		}
	}
	if idx <= 3 {
		if err := e.phase(a, PhaseRestoreStart); err != nil {
			return err
		}
		if err := e.waitStable(ctx); err != nil {
			return pending(err)
		}
		if err := e.ensurePrevious(ctx, a); err != nil {
			if codeOf(err) == "previous_image_changed" {
				return needRecovery("previous_image_changed")
			}
			return pending(err)
		}
		st, err := e.appState(ctx)
		if err != nil {
			return pending(fail("middleware_unavailable", err))
		}
		if st != AppRunning {
			if err := e.runJob(ctx, a, "restore_start", func(c context.Context) (int, error) { return e.MW.Start(c, a.App) }); err != nil {
				return pending(err)
			}
		}
	}
	if err := e.phase(a, PhaseRestoreVerify); err != nil {
		return err
	}
	if err := e.verify(ctx, a, a.PreviousImageID, e.Timeouts.Sustain); err != nil {
		if ctx.Err() != nil {
			return errPending
		}
		return needRecovery("previous_not_ready")
	}
	st, err := e.Journal.LoadState()
	if err != nil {
		return fail("journal_unreadable", err)
	}
	if a.CandidateStarted && a.Reason != "interrupted" && !slices.Contains(st.Quarantine, a.CandidateDigest) {
		st.Quarantine = append(st.Quarantine, a.CandidateDigest)
	}
	// The stored image is a digest now: remember which release it is.
	st.LastTag = a.FromTag
	if err := e.Journal.SaveState(st); err != nil {
		return fail("journal_unwritable", err)
	}
	if err := e.phase(a, PhaseRolledBack); err != nil {
		return err
	}
	e.Journal.DropCompose()
	e.dropSnapshot(ctx, a)
	return nil
}

// ensurePrevious confirms the pinned previous image is local and still the
// image id the attempt bound.
func (e *Executor) ensurePrevious(ctx context.Context, a *Attempt) error {
	get := func() (string, error) {
		c, cancel := e.cc(ctx)
		defer cancel()
		return e.Docker.ImageID(c, a.PreviousRef)
	}
	id, err := get()
	if errors.Is(err, ErrNoImage) {
		if perr := e.pull(ctx, a.PreviousRef); perr != nil {
			return fail("previous_image_pull_failed", perr)
		}
		id, err = get()
	}
	if err != nil {
		return fail("docker_unavailable", err)
	}
	if id != a.PreviousImageID {
		return fail("previous_image_changed", nil)
	}
	return nil
}

// waitStable waits (bounded) while the app is DEPLOYING. TrueNAS reports
// DEPLOYING for a running container whose health check stays unhealthy, so
// a timeout is not an error: the jobs are settled and recovery goes on.
func (e *Executor) waitStable(ctx context.Context) error {
	err := e.poll(ctx, e.Timeouts.Deploy, "app_busy", func() (bool, error) {
		s, err := e.appState(ctx)
		if err != nil {
			return false, fail("middleware_unavailable", err)
		}
		return s != AppDeploying, nil
	})
	if codeOf(err) == "app_busy" {
		return nil
	}
	return err
}

// reconcile finishes or undoes an unfinished attempt of an earlier run.
func (e *Executor) reconcile(ctx context.Context, a *Attempt) error {
	// 0. Never race a middleware job of the interrupted run.
	if err := e.settleJobs(ctx, a); err != nil {
		return err
	}
	if err := e.waitStable(ctx); err != nil {
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
	case PhaseStarting, PhaseVerifying, PhaseFinalizing:
		// The candidate may be fine: a run killed after a good start only
		// lost its commit. Prove it again, then carry on forward.
		if e.candidateRunning(ctx, a) {
			sustain := e.Timeouts.Sustain
			if a.Phase == PhaseFinalizing {
				sustain = e.Timeouts.Final
			}
			if verr := e.verify(ctx, a, a.CandidateImageID, sustain); verr == nil {
				if _, after, _, derr := e.docs(a); derr == nil {
					if ferr := e.finalize(ctx, a, after); ferr == nil {
						return nil
					} else {
						a.Reason = codeOf(ferr)
					}
				}
			} else {
				a.Reason = codeOf(verr)
			}
		}
	}
	reason := a.Reason
	if !restorePhase(a.Phase) && reason == "" {
		reason = "interrupted"
	}
	return e.restore(ctx, a, reason)
}

// untouched: the app still runs the previous release with a document the
// attempt may have written, so nothing needs undoing.
func (e *Executor) untouched(ctx context.Context, a *Attempt) bool {
	st, err := e.appState(ctx)
	if err != nil || st != AppRunning {
		return false
	}
	cur, err := e.appConfig(ctx)
	if err != nil || cur.Hash() != a.ComposeBeforeHash {
		return false
	}
	cs, err := e.containers(ctx)
	if err != nil {
		return false
	}
	c := runningOf(cs, a.Service)
	return c != nil && c.ImageID == a.PreviousImageID
}

func (e *Executor) candidateRunning(ctx context.Context, a *Attempt) bool {
	st, err := e.appState(ctx)
	if err != nil || st == AppStopped {
		return false
	}
	cs, err := e.containers(ctx)
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
