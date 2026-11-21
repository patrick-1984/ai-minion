package truenas

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/patrick-1984/ai-minion/internal/version"
)

// Executor runs one tick of the host updater.
type Executor struct {
	Cfg      Config
	MW       Middleware
	Docker   Docker
	ZFS      ZFS
	Releases ReleaseSource
	Probe    Prober
	Journal  *Journal
	Timeouts Timeouts
	Log      *slog.Logger
	// Now and Sleep are replaced in tests.
	Now   func() time.Time
	Sleep func(ctx context.Context, d time.Duration) error

	lastRequestError, lastRequestHandled string
}

// Tick outcomes (Status.Tick).
const (
	TickBusy             = "busy"
	TickIdle             = "idle"
	TickDisabled         = "disabled"
	TickOutsideWindow    = "outside_window"
	TickHubDown          = "hub_down"
	TickUpToDate         = "up_to_date"
	TickUpdated          = "updated"
	TickRolledBack       = "rolled_back"
	TickReconciled       = "reconciled"
	TickPending          = "reconcile_pending"
	TickRecoveryRequired = "recovery_required"
	TickRefused          = "refused"
)

// TickResult is what a tick did.
type TickResult struct {
	Tick    string
	Detail  string
	Attempt *Attempt
}

// stepError is a failure with a short code that is safe to publish.
type stepError struct {
	Code string
	Err  error
}

func (e *stepError) Error() string {
	if e.Err != nil {
		return e.Code + ": " + e.Err.Error()
	}
	return e.Code
}
func (e *stepError) Unwrap() error { return e.Err }

func fail(code string, err error) error { return &stepError{Code: code, Err: err} }

func codeOf(err error) string {
	var se *stepError
	if errors.As(err, &se) {
		return se.Code
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "timeout"
	}
	return "error"
}

func (e *Executor) now() time.Time {
	if e.Now != nil {
		return e.Now()
	}
	return time.Now()
}

func (e *Executor) sleep(ctx context.Context, d time.Duration) error {
	if e.Sleep != nil {
		return e.Sleep(ctx, d)
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func (e *Executor) logger() *slog.Logger {
	if e.Log != nil {
		return e.Log
	}
	return slog.New(slog.DiscardHandler)
}

func (e *Executor) project() string { return "ix-" + e.Cfg.App }

// Tick performs one run. The order is fixed: lock, reconcile, request,
// policy and window, attempt. It writes the status file before returning.
func (e *Executor) Tick(ctx context.Context) (TickResult, error) {
	unlock, err := e.Journal.Lock()
	if errors.Is(err, ErrBusy) {
		return TickResult{Tick: TickBusy}, nil
	}
	if err != nil {
		return TickResult{}, err
	}
	defer unlock()

	res, tickErr := e.tick(ctx)
	st, serr := e.Journal.LoadState()
	if serr == nil {
		att, _ := e.Journal.LoadAttempt()
		e.publish(res, st, att, e.lastRequestError, e.lastRequestHandled)
	}
	return res, tickErr
}

func (e *Executor) publish(res TickResult, st State, att *Attempt, reqErr, handled string) {
	s := Status{
		App: e.Cfg.App, UpdatedAt: e.now().UTC(), Tick: res.Tick, Detail: res.Detail,
		CurrentTag: st.LastTag, Window: e.Cfg.Window, Channel: e.Cfg.Channel, Enabled: e.Cfg.Enabled,
		Quarantined: st.Quarantine, LastAttempt: attemptStatus(att), RequestError: reqErr, RequestHandled: handled,
	}
	if err := WriteStatus(e.Cfg.StatusFile, s); err != nil {
		e.logger().Error("status file not written", "error", err.Error())
	}
}

func (e *Executor) tick(ctx context.Context) (TickResult, error) {
	e.lastRequestError, e.lastRequestHandled = "", ""

	// 1. Reconcile first: outside the window, disabled, Hub down.
	att, err := e.Journal.LoadAttempt()
	if err != nil {
		return TickResult{Tick: TickRefused, Detail: "journal_unreadable"}, err
	}
	if att != nil && !Terminal(att.Phase) {
		if err := e.reconcile(ctx, att); err != nil {
			if errors.Is(err, errPending) {
				return TickResult{Tick: TickPending, Detail: att.Phase, Attempt: att}, nil
			}
			return TickResult{Tick: TickRecoveryRequired, Detail: codeOf(err), Attempt: att}, err
		}
		tick := TickReconciled
		if att.Phase == PhaseRecoveryRequired {
			tick = TickRecoveryRequired
		}
		return TickResult{Tick: tick, Detail: att.Phase, Attempt: att}, nil
	}
	if att != nil && Terminal(att.Phase) {
		e.sweep(ctx, att)
	}
	if att != nil && att.Phase == PhaseRecoveryRequired {
		return TickResult{Tick: TickRecoveryRequired, Detail: att.Reason, Attempt: att}, nil
	}

	st, err := e.Journal.LoadState()
	if err != nil {
		return TickResult{Tick: TickRefused, Detail: "journal_unreadable"}, err
	}

	// 2. Request file (untrusted input; problems never stop the reconcile above).
	now := e.now()
	req, rerr := ReadRequest(e.Cfg.RequestFile, now)
	if rerr != nil {
		e.lastRequestError = rerr.Error()
		req = nil
	}
	enabled, channel, window := e.Cfg.Enabled, e.Cfg.Channel, e.Cfg.Window
	if req != nil && req.Policy != nil && e.Cfg.AllowHubPolicy {
		enabled, channel, window = req.Policy.Enabled, req.Policy.Channel, req.Policy.Window
	}
	var once *Once
	if req != nil && req.Once != nil && !slices.Contains(st.Consumed, req.Once.ID) {
		once = req.Once
	} else if req != nil && req.Once != nil {
		e.lastRequestHandled = req.Once.ID
	}

	// 3. Admission.
	if once == nil {
		if !enabled {
			return TickResult{Tick: TickDisabled}, nil
		}
		w, werr := ParseWindow(window)
		if werr != nil {
			return TickResult{Tick: TickRefused, Detail: "bad_window"}, nil
		}
		if !w.Contains(now) {
			return TickResult{Tick: TickOutsideWindow}, nil
		}
	}

	state, err := e.MW.AppState(ctx, e.Cfg.App)
	if err != nil {
		return TickResult{Tick: TickRefused, Detail: "middleware_unavailable"}, nil
	}
	if state != AppRunning {
		return TickResult{Tick: TickHubDown, Detail: strings.ToLower(state)}, nil
	}
	return e.attempt(ctx, st, channel, once)
}

// attempt selects a release and runs the transaction.
func (e *Executor) attempt(ctx context.Context, st State, channel string, once *Once) (TickResult, error) {
	refuse := func(code string) (TickResult, error) { return TickResult{Tick: TickRefused, Detail: code}, nil }

	compose, err := e.MW.AppConfig(ctx, e.Cfg.App)
	if err != nil {
		return refuse("middleware_unavailable")
	}
	runningRef, err := compose.Image(e.Cfg.Service)
	if err != nil {
		return refuse("compose_service_missing")
	}

	baseTag := ""
	switch {
	case once != nil:
		baseTag = once.Baseline
	case st.LastTag != "":
		baseTag = st.LastTag
	default:
		baseTag = tagOfRef(runningRef)
	}
	baseVer, err := version.ParseVersion(baseTag)
	if err != nil || baseVer.String() != baseTag || baseVer.IsPrerelease() {
		return refuse("baseline_unknown")
	}

	selector := channel
	if once != nil {
		selector = once.Target
	}
	cand, err := e.Releases.Verified(ctx, selector)
	if err != nil {
		return refuse("release_unverified")
	}
	candVer, err := version.ParseVersion(cand.Tag)
	if err != nil || cand.Prerelease || candVer.IsPrerelease() {
		return refuse("candidate_not_stable")
	}
	if once != nil && cand.Tag != once.Target {
		return refuse("candidate_not_target")
	}
	if candVer.Compare(baseVer) <= 0 {
		e.consume(&st, once)
		return TickResult{Tick: TickUpToDate, Detail: baseTag}, nil
	}
	if slices.Contains(st.Quarantine, cand.Digest) {
		return refuse("candidate_quarantined")
	}
	base, err := e.Releases.Verified(ctx, baseTag)
	if err != nil {
		return refuse("baseline_unverified")
	}
	if base.Tag != baseTag {
		return refuse("baseline_unverified")
	}
	if !rangeContains(cand.Range, base.Range) {
		return refuse("protocol_incompatible")
	}

	// Pre-pull both references and bind them to local image ids.
	if err := e.pull(ctx, cand.Ref()); err != nil {
		return refuse("pull_candidate_failed")
	}
	if _, err := e.Docker.ImageID(ctx, base.Ref()); err != nil {
		if err := e.pull(ctx, base.Ref()); err != nil {
			return refuse("pull_previous_failed")
		}
	}
	prevID, err := e.Docker.ImageID(ctx, base.Ref())
	if err != nil {
		return refuse("previous_image_missing")
	}
	candID, err := e.Docker.ImageID(ctx, cand.Ref())
	if err != nil {
		return refuse("candidate_image_missing")
	}
	if prevID == candID {
		return refuse("candidate_equals_previous")
	}
	cs, err := e.Docker.Containers(ctx, e.project())
	if err != nil {
		return refuse("docker_unavailable")
	}
	run := runningOf(cs, e.Cfg.Service)
	if run == nil || run.ImageID != prevID {
		return refuse("running_image_mismatch")
	}
	if err := e.Probe.Ready(ctx); err != nil {
		return TickResult{Tick: TickHubDown, Detail: "not_ready"}, nil
	}
	zctx, cancel := context.WithTimeout(ctx, e.Timeouts.ZFS)
	free, err := e.ZFS.Available(zctx, e.Cfg.Dataset)
	cancel()
	if err != nil {
		return refuse("zfs_unavailable")
	}
	if free < e.Cfg.MinFreeBytes {
		return refuse("not_enough_space")
	}
	cur, err := e.MW.AppConfig(ctx, e.Cfg.App)
	if err != nil || cur.Hash() != compose.Hash() {
		return refuse("compose_changed")
	}
	after, err := compose.WithImage(e.Cfg.Service, cand.Ref())
	if err != nil {
		return refuse("compose_service_missing")
	}

	a := &Attempt{
		ID: NewAttemptID(), App: e.Cfg.App, Service: e.Cfg.Service, Phase: PhasePrepared,
		FromTag: baseTag, ToTag: cand.Tag, PreviousRef: runningRef, CandidateRef: cand.Ref(),
		CandidateDigest: cand.Digest, PreviousImageID: prevID, CandidateImageID: candID,
		Dataset: e.Cfg.Dataset, ComposeBeforeHash: compose.Hash(), ComposeAfterHash: after.Hash(),
		Snapshot:  snapshotName(cand.Tag, ""),
		StartedAt: e.now().UTC(),
	}
	a.Snapshot = snapshotName(cand.Tag, a.ID)
	if once != nil {
		a.RequestID = once.ID
	}
	// The saved compose first, then the attempt, then the consumption of the
	// one-shot request: all durable before the first disruption.
	if err := e.Journal.SaveCompose(compose); err != nil {
		return refuse("journal_unwritable")
	}
	if err := e.Journal.SaveAttempt(a); err != nil {
		return refuse("journal_unwritable")
	}
	if once != nil {
		e.consume(&st, once)
		if err := e.Journal.SaveState(st); err != nil {
			return refuse("journal_unwritable")
		}
		e.lastRequestHandled = once.ID
	}

	terr := e.transact(ctx, a, after)
	if terr == nil {
		return TickResult{Tick: TickUpdated, Detail: cand.Tag, Attempt: a}, nil
	}
	e.logger().Warn("update failed; restoring", "attempt", a.ID, "code", codeOf(terr))
	if err := e.restore(ctx, a, codeOf(terr)); err != nil {
		if errors.Is(err, errPending) {
			return TickResult{Tick: TickPending, Detail: a.Phase, Attempt: a}, nil
		}
		return TickResult{Tick: TickRecoveryRequired, Detail: codeOf(err), Attempt: a}, err
	}
	if a.Phase == PhaseRecoveryRequired {
		return TickResult{Tick: TickRecoveryRequired, Detail: a.Reason, Attempt: a}, nil
	}
	return TickResult{Tick: TickRolledBack, Detail: a.Reason, Attempt: a}, nil
}

func (e *Executor) consume(st *State, once *Once) {
	if once == nil || slices.Contains(st.Consumed, once.ID) {
		return
	}
	st.Consumed = append(st.Consumed, once.ID)
	_ = e.Journal.SaveState(*st)
	e.lastRequestHandled = once.ID
}

func (e *Executor) pull(ctx context.Context, ref string) error {
	c, cancel := context.WithTimeout(ctx, e.Timeouts.Pull)
	defer cancel()
	return e.Docker.Pull(c, ref)
}

func rangeContains(outer, inner version.ProtoRange) bool {
	return outer.Min.Major == inner.Min.Major &&
		outer.Min.Compare(inner.Min) <= 0 && outer.Max.Compare(inner.Max) >= 0
}

func runningOf(cs []Container, service string) *Container {
	var found *Container
	for i := range cs {
		if cs[i].Service == service && cs[i].Running {
			if found != nil {
				return nil // two running containers: not provable
			}
			found = &cs[i]
		}
	}
	return found
}

// tagOfRef returns the tag of "repo:tag" (a digest reference has none).
func tagOfRef(ref string) string {
	if strings.Contains(ref, "@") {
		return ""
	}
	i := strings.LastIndex(ref, ":")
	if i < 0 || strings.Contains(ref[i:], "/") {
		return ""
	}
	return ref[i+1:]
}

func snapshotName(tag, id string) string {
	return fmt.Sprintf("minion-pre-%s-%s", tag, id)
}
