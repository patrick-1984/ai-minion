package truenas

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"path/filepath"
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

// call wrappers: every external call gets its own deadline.
func (e *Executor) cc(ctx context.Context) (context.Context, context.CancelFunc) {
	if e.Timeouts.Call <= 0 {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, e.Timeouts.Call)
}

func (e *Executor) appState(ctx context.Context) (string, error) {
	c, cancel := e.cc(ctx)
	defer cancel()
	return e.MW.AppState(c, e.Cfg.App)
}

func (e *Executor) appConfig(ctx context.Context) (Compose, error) {
	c, cancel := e.cc(ctx)
	defer cancel()
	return e.MW.AppConfig(c, e.Cfg.App)
}

func (e *Executor) containers(ctx context.Context) ([]Container, error) {
	c, cancel := e.cc(ctx)
	defer cancel()
	return e.Docker.Containers(c, e.project())
}

func (e *Executor) jobState(ctx context.Context, id int) (JobState, error) {
	c, cancel := e.cc(ctx)
	defer cancel()
	return e.MW.JobState(c, id)
}

func (e *Executor) zfsCall(ctx context.Context, f func(context.Context) error) error {
	c, cancel := context.WithTimeout(ctx, e.Timeouts.ZFS)
	defer cancel()
	return f(c)
}

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
		st = e.syncLastTag(st, att)
		e.publish(res, st, att, e.lastRequestError, e.lastRequestHandled)
	}
	return res, tickErr
}

// syncLastTag derives the installed tag from the single journal record that
// says an attempt committed (the commit writes nothing else), and repairs
// the state when a run died between the two.
func (e *Executor) syncLastTag(st State, att *Attempt) State {
	if att != nil && att.Phase == PhaseCommitted && st.LastTag != att.ToTag {
		st.LastTag = att.ToTag
		if err := e.Journal.SaveState(st); err != nil {
			e.logger().Error("state not written", "error", err.Error())
		}
	}
	return st
}

func (e *Executor) publish(res TickResult, st State, att *Attempt, reqErr, handled string) {
	s := Status{
		App: e.Cfg.App, UpdatedAt: e.now().UTC(), Tick: res.Tick, Detail: res.Detail,
		CurrentTag: st.LastTag, Window: e.Cfg.Window, Channel: e.Cfg.Channel, Enabled: e.Cfg.Enabled, AllowHubPolicy: e.Cfg.AllowHubPolicy,
		Quarantined: st.Quarantine, LastAttempt: attemptStatus(att), RequestError: reqErr, RequestHandled: handled,
	}
	if req, _ := ReadRequest(e.Cfg.RequestFile, e.now()); req != nil && req.Policy != nil && e.Cfg.AllowHubPolicy {
		s.Enabled, s.Window, s.Channel = req.Policy.Enabled, req.Policy.Window, req.Policy.Channel
	}
	if err := WriteStatus(e.Cfg.StatusFile, s); err != nil {
		e.logger().Error("status file not written", "error", err.Error())
	}
}

// admission is the effective permission to start an attempt now.
type admission struct {
	channel string
	once    *Once
}

// admit evaluates the request file, the policy and the window against the
// current time. It returns a result when no attempt may start.
func (e *Executor) admit(st State) (admission, *TickResult) {
	now := e.now()
	req, rerr := ReadRequest(e.Cfg.RequestFile, now)
	if rerr != nil {
		e.lastRequestError = rerr.Error()
	}
	enabled, channel, window := e.Cfg.Enabled, e.Cfg.Channel, e.Cfg.Window
	if req != nil && req.Policy != nil && e.Cfg.AllowHubPolicy {
		enabled, channel, window = req.Policy.Enabled, req.Policy.Channel, req.Policy.Window
	}
	var once *Once
	if req != nil && req.Once != nil {
		if slices.Contains(st.Consumed, req.Once.ID) {
			e.lastRequestHandled = req.Once.ID
		} else {
			once = req.Once
		}
	}
	if once != nil {
		return admission{channel: channel, once: once}, nil
	}
	if !enabled {
		return admission{}, &TickResult{Tick: TickDisabled}
	}
	w, werr := ParseWindow(window)
	if werr != nil {
		return admission{}, &TickResult{Tick: TickRefused, Detail: "bad_window"}
	}
	if !w.Contains(now) {
		return admission{}, &TickResult{Tick: TickOutsideWindow}
	}
	return admission{channel: channel}, nil
}

func (e *Executor) tick(ctx context.Context) (TickResult, error) {
	e.lastRequestError, e.lastRequestHandled = "", ""

	att, err := e.Journal.LoadAttempt()
	if err != nil {
		return TickResult{Tick: TickRefused, Detail: "journal_unreadable"}, err
	}
	st, err := e.Journal.LoadState()
	if err != nil {
		return TickResult{Tick: TickRefused, Detail: "journal_unreadable"}, err
	}
	st = e.syncLastTag(st, att)

	// 1. Reconcile first: outside the window, disabled, Hub down.
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
		_ = e.syncLastTag(st, att)
		return TickResult{Tick: tick, Detail: att.Phase, Attempt: att}, nil
	}
	if att != nil && Terminal(att.Phase) {
		e.sweep(ctx, att)
	}
	if att != nil && att.Phase == PhaseRecoveryRequired {
		return TickResult{Tick: TickRecoveryRequired, Detail: att.Reason, Attempt: att}, nil
	}

	// 2. Request file (untrusted input), policy and window.
	adm, res := e.admit(st)
	if res != nil {
		return *res, nil
	}

	state, err := e.appState(ctx)
	if err != nil {
		return TickResult{Tick: TickRefused, Detail: "middleware_unavailable"}, nil
	}
	if state != AppRunning {
		return TickResult{Tick: TickHubDown, Detail: strings.ToLower(state)}, nil
	}
	return e.attempt(ctx, st, adm)
}

// attempt selects a release and runs the transaction.
func (e *Executor) attempt(ctx context.Context, st State, adm admission) (TickResult, error) {
	refuse := func(code string) (TickResult, error) { return TickResult{Tick: TickRefused, Detail: code}, nil }
	once := adm.once

	custom, err := e.MW.IsCustom(ctx, e.Cfg.App)
	if err != nil {
		return refuse("middleware_unavailable")
	}
	if !custom {
		return refuse("not_a_custom_app")
	}
	compose, err := e.appConfig(ctx)
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

	selector := adm.channel
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
	// Earlier signed releases do not implement the health-only trial boot.
	// Merely setting their environment would still admit acknowledged writes.
	trialSince, _ := version.ParseVersion("v0.17.0")
	if candVer.Compare(trialSince) < 0 {
		return refuse("candidate_has_no_trial_fence")
	}
	if slices.Contains(st.Quarantine, cand.Digest) {
		return refuse("candidate_quarantined")
	}
	base, err := e.Releases.Verified(ctx, baseTag)
	if err != nil || base.Tag != baseTag {
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
	cs, err := e.containers(ctx)
	if err != nil {
		return refuse("docker_unavailable")
	}
	run := runningOf(cs, e.Cfg.Service)
	if run == nil || run.ImageID != prevID {
		return refuse("running_image_mismatch")
	}
	// Bind the checkpoint to where the running Hub really writes, not just
	// another valid bind mount named by a stale updater configuration. Require
	// the same explicit literal in stored Compose so env_file/interpolation
	// changes cannot redirect the candidate between stop and start.
	wantDir := filepath.Clean(e.Cfg.DataMount)
	if !filepath.IsAbs(run.DataDir) || filepath.Clean(run.DataDir) != wantDir ||
		compose.Env(e.Cfg.Service, "MINION_DATA_DIR") != wantDir {
		return refuse("hub_data_mount_mismatch")
	}

	// The configured dataset must be the source of the Hub's data mount.
	mctx, mcancel := e.cc(ctx)
	src, err := e.Docker.MountSource(mctx, run.ID, wantDir)
	mcancel()
	if err != nil {
		return refuse("data_mount_unknown")
	}
	var info DatasetInfo
	if err := e.zfsCall(ctx, func(c context.Context) (err error) { info, err = e.ZFS.DatasetAt(c, src); return }); err != nil {
		return refuse("dataset_unresolved")
	}
	if info.Name != e.Cfg.Dataset || info.GUID == "" {
		return refuse("dataset_mismatch")
	}

	scheme, port := "https", "8443"
	if compose.Env(e.Cfg.Service, "MINION_TLS") == "off" {
		scheme = "http"
	}
	if l := compose.Env(e.Cfg.Service, "MINION_LISTEN"); strings.Contains(l, ":") && !strings.HasSuffix(l, ":") {
		port = l[strings.LastIndex(l, ":")+1:]
	}
	a := &Attempt{
		ID: NewAttemptID(), App: e.Cfg.App, Service: e.Cfg.Service, Phase: PhasePrepared,
		FromTag: baseTag, ToTag: cand.Tag, PreviousRef: base.Ref(), CandidateRef: cand.Ref(),
		CandidateDigest: cand.Digest, PreviousImageID: prevID, CandidateImageID: candID,
		Dataset: e.Cfg.Dataset, DataPath: src, DatasetGUID: info.GUID,
		ProbeScheme: scheme, ProbePort: port, ComposeBeforeHash: compose.Hash(),
		StartedAt: e.now().UTC(),
	}
	if err := e.readyNow(ctx, a, run); err != nil {
		return TickResult{Tick: TickHubDown, Detail: "not_ready"}, nil
	}
	var free uint64
	if err := e.zfsCall(ctx, func(c context.Context) (err error) { free, err = e.ZFS.Available(c, e.Cfg.Dataset); return }); err != nil {
		return refuse("zfs_unavailable")
	}
	if free < e.Cfg.MinFreeBytes {
		return refuse("not_enough_space")
	}
	cur, err := e.appConfig(ctx)
	if err != nil || cur.Hash() != compose.Hash() {
		return refuse("compose_changed")
	}
	// The restore document pins the VERIFIED previous digest, never a tag.
	restoreDoc, err := compose.WithImage(e.Cfg.Service, base.Ref())
	if err != nil {
		return refuse("compose_service_missing")
	}
	after, err := compose.WithImage(e.Cfg.Service, cand.Ref())
	if err != nil {
		return refuse("compose_service_missing")
	}
	a.SavedHash, a.ComposeAfterHash = restoreDoc.Hash(), after.Hash()
	a.Snapshot = snapshotName(cand.Tag, a.ID)
	if once != nil {
		a.RequestID = once.ID
	}

	// Policy and one-shot are checked again immediately before the attempt
	// that may stop the app is recorded (pulls and verification take time).
	adm2, res := e.admit(st)
	if res != nil || adm2.channel != adm.channel || (adm2.once == nil) != (once == nil) ||
		(once != nil && *adm2.once != *once) {
		return refuse("admission_changed")
	}
	// The saved document first, then the attempt, then the consumption of the
	// one-shot request: all durable before the first disruption.
	if err := e.Journal.SaveCompose(restoreDoc); err != nil {
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

	terr := e.transact(ctx, a)
	if terr == nil {
		return TickResult{Tick: TickUpdated, Detail: cand.Tag, Attempt: a}, nil
	}
	if a.KeepCandidate {
		// Writers may already have acknowledged data. Retry forward on the
		// next tick; reverting the checkpoint is no longer safe.
		return TickResult{Tick: TickPending, Detail: a.Phase, Attempt: a}, nil
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

// readyURL is the /readyz URL of the container itself (never a configured
// URL that could reach another Hub).
func readyURL(a *Attempt, c *Container) string {
	return a.ProbeScheme + "://" + net.JoinHostPort(c.IP, a.ProbePort) + "/readyz"
}

func (e *Executor) readyNow(ctx context.Context, a *Attempt, c *Container) error {
	if c.IP == "" {
		return errors.New("no container address")
	}
	pctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return e.Probe.Ready(pctx, readyURL(a, c))
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
