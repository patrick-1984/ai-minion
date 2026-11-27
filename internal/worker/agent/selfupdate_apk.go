package agent

// The Android app's self-update, Worker side (v0.24.0, design WP2). The binary
// inside the app is never replaced; instead the Worker downloads and verifies
// the signed APK of one exact release, stages it in the app's private storage
// and hands it to the app (the Kotlin updater), which installs it with
// PackageInstaller. Installing kills every process of the app, so the Worker
// hands over only when no agent is busy and no client is attached to a session
// (Minion's own session state; Android's InstallConstraints count the Worker's
// keepalives as interaction), unless the Hub asks for "now".
//
// Handover (files in <state>/update, the app's private storage; every write is
// a write of a temporary file and a rename):
//
//	apk/minion-android-X.Y.Z.apk     the staged APK (0400), with the signed
//	apk/checksums.txt(.sigstore.json) checksums.txt and its Sigstore bundle
//	apk-pending.json                 Worker -> app: "install this", see apkRequest.
//	                                 The Worker removes it to withdraw the request.
//	apk-status.json                  app -> Worker: readiness and the attempt of
//	                                 the request it took, see apkAppState (the
//	                                 app's ApkUpdater.kt writes it).
//
// The app must not trust the digest in apk-request.json alone (every terminal
// session runs as the app's user and could rewrite both files): it runs
// "libminion.so worker apk-verify --tag <tag> --json", which authenticates the
// staged checksums.txt with the Sigstore signature of the release workflow for
// that tag, and requires the bytes it streams into the install session to have
// that digest, the package cc.nasz.minion, version_code above the installed one
// and the pinned signing certificate. Right before commit it reads the request
// again: same id, not expired, else it abandons the session.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
	"github.com/patrick-1984/ai-minion/internal/version"
	"github.com/patrick-1984/ai-minion/internal/worker/agentdetect"
)

// Names of the handover files in <state>/update.
const (
	apkUpdateDir   = "update"
	apkStageDir    = "apk"
	apkRequestFile = "apk-pending.json"
	apkAppFile     = "apk-status.json"
	// apkPackage is the Android package the APK must carry.
	apkPackage = "cc.nasz.minion"
)

// Timings of the APK handover.
const (
	// apkPoll is how often the Worker checks idleness and the app's answer
	// (the app reads the request on its one-second tick).
	apkPoll = 2 * time.Second
	// apkHandoverTTL is how long the app may take to start the install of a
	// request; the request carries it as expires_at.
	apkHandoverTTL = 2 * time.Minute
	// apkIdleWaitMax bounds the wait for an idle moment.
	apkIdleWaitMax = 24 * time.Hour
	// apkInstallMax bounds the wait for the outcome once the app took it
	// (a confirmation on the device included).
	apkInstallMax = 24 * time.Hour
)

// apkRequest is apk-request.json.
type apkRequest struct {
	Schema int `json:"schema"`
	// ID names this attempt; the app echoes it in apk-app.json.
	ID string `json:"id"`
	// Tag is the release, Version its version and VersionCode the
	// versionCode the APK must carry.
	Tag         string `json:"tag"`
	Version     string `json:"version"`
	VersionCode int64  `json:"version_code"`
	// From is the running version and FromVersionCode its code.
	From            string `json:"from"`
	FromVersionCode int64  `json:"from_version_code"`
	Package         string `json:"package"`
	// APK is the staged file, SHA256 and Size what the signed checksums say.
	APK       string `json:"path"`
	SHA256    string `json:"sha256"`
	Size      int64  `json:"size"`
	Checksums string `json:"checksums"`
	Bundle    string `json:"bundle"`
	// Now: the Hub asked to install at once, sessions or not. Approved is
	// always true: the Worker writes the request only once no agent is busy
	// and no client is attached, or when the Hub asked for now.
	Now       bool      `json:"now"`
	Approved  bool      `json:"approved"`
	CreatedAt time.Time `json:"created_at"`
	ExpiresAt time.Time `json:"expires_at"`
}

// apkAppState is apk-status.json, written by the app (the fields the Worker
// reads; the app writes more).
type apkAppState struct {
	Schema int `json:"schema"`
	// Ready says whether the app can install an update now; Reason
	// (block_reason) says why not, for example "Install unknown apps is not
	// allowed for Minion" or "updates are turned off on this device".
	Ready  bool   `json:"ready"`
	Reason string `json:"block_reason,omitempty"`
	// InstalledVersionCode is the versionCode of the installed app.
	InstalledVersionCode int64 `json:"app_version_code,omitempty"`
	// Attempt is the request the app took.
	Attempt *apkAppAttempt `json:"attempt,omitempty"`
}

// apkAppAttempt is the attempt object of apk-status.json.
type apkAppAttempt struct {
	ID     string `json:"id"`
	State  string `json:"state"`
	Reason string `json:"reason,omitempty"`
}

// attempt returns the state and reason the app reports for request id.
func (st apkAppState) attempt(id string) (state, reason string, ok bool) {
	if st.Attempt == nil || st.Attempt.ID != id {
		return "", "", false
	}
	return st.Attempt.State, st.Attempt.Reason, true
}

// Attempt states of apk-status.json (ApkUpdater.kt).
const (
	apkAppInstalling        = "installing"
	apkAppNeedsConfirmation = "needs_confirmation_on_device"
	apkAppFailed            = "failed"
	apkAppInstalled         = "installed"
)

// apkAppFinal is every attempt state that ends an attempt without an install.
var apkAppFinal = map[string]bool{apkAppFailed: true, "refused": true, "declined": true, "interrupted": true,
	"unknown": true, "blocked": true, "off": true}

// apkUpdater is the Android part of selfUpdate.
type apkUpdater struct {
	// dir is <state>/update.
	dir string
	// stage downloads, verifies and stages the APK (*version.Updater.StageAPK).
	stage func(ctx context.Context, o version.APKOptions) (version.StagedAPK, error)
	// idle reports whether no agent is busy and no client is attached, or
	// why not.
	idle func(ctx context.Context) (bool, string)
	// poll, handoverTTL, idleMax and installMax override the timings (tests).
	poll, handoverTTL, idleMax, installMax time.Duration
}

func (a *apkUpdater) timing(d, def time.Duration) time.Duration {
	if d > 0 {
		return d
	}
	return def
}

func (a *apkUpdater) stageDir() string { return filepath.Join(a.dir, apkStageDir) }

// appState reads apk-app.json; ok is false when the app has not written it.
func (a *apkUpdater) appState() (apkAppState, bool) {
	b, err := os.ReadFile(filepath.Join(a.dir, apkAppFile))
	if err != nil || len(b) > 64<<10 {
		return apkAppState{}, false
	}
	var st apkAppState
	if json.Unmarshal(b, &st) != nil {
		return apkAppState{}, false
	}
	return st, true
}

// block says why the app cannot install an update now, "" when it can.
func (a *apkUpdater) block() string {
	st, ok := a.appState()
	switch {
	case !ok:
		return "the Minion app has not reported that it can install updates yet; open the app once"
	case !st.Ready && st.Reason != "":
		return "the Minion app cannot install updates: " + proto.SafeText(st.Reason, 300)
	case !st.Ready:
		return "the Minion app cannot install updates now"
	}
	return ""
}

func (a *apkUpdater) writeRequest(r apkRequest) error {
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	tmp := filepath.Join(a.dir, "."+apkRequestFile+".tmp")
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(a.dir, apkRequestFile))
}

func (a *apkUpdater) readRequest() (apkRequest, bool) {
	b, err := os.ReadFile(filepath.Join(a.dir, apkRequestFile))
	if err != nil {
		return apkRequest{}, false
	}
	var r apkRequest
	return r, json.Unmarshal(b, &r) == nil
}

// withdraw removes the request, so the app installs nothing more of it.
func (a *apkUpdater) withdraw() { _ = os.Remove(filepath.Join(a.dir, apkRequestFile)) }

// startUp runs once when the Worker starts: a request of an earlier run is
// withdrawn (that run can no longer watch it), and its outcome is reported.
func (a *apkUpdater) startUp(u *selfUpdate) {
	req, ok := a.readRequest()
	a.withdraw()
	if !ok {
		return
	}
	switch {
	case req.Tag != "" && apkTagVersion(req.Tag) == version.Version:
		u.log.Info("the Minion app was updated", "from", req.From, "to", version.Version)
		version.RemoveStagedAPK(a.stageDir())
	default:
		if st, ok := a.appState(); ok {
			if state, reason, ok := st.attempt(req.ID); ok && apkAppFinal[state] {
				u.setState(ops.UpdateStateFailed, req.From, req.Tag, "the app did not install the update ("+state+"): "+reason)
			}
		}
		version.RemoveStagedAPK(a.stageDir())
	}
}

// apkTagVersion is the version a tag installs: "android-vX.Y.Z" installs
// vX.Y.Z.
func apkTagVersion(tag string) string {
	v, _, err := version.ParseReleaseTag(tag)
	if err != nil {
		return ""
	}
	return v.String()
}

// handleAPKUpdate is worker_update on the Android app.
func (u *selfUpdate) handleAPKUpdate(ctx context.Context, args ops.WorkerUpdateArgs) (any, error) {
	res := ops.WorkerUpdateResult{From: version.Version, To: args.Version}
	target, _, err := version.ParseReleaseTag(args.Version)
	if err != nil {
		return nil, apiErr(proto.CodeInvalidArgument, "version", "version must be one exact release such as v0.24.0 or android-v0.24.1 (no channel)")
	}
	if b := u.blockReason(); b != "" {
		res.Reason = b
		return res, nil
	}
	cur, err := version.ParseVersion(version.Version)
	if err != nil {
		res.Reason = "this Worker is a development build; install the APK by hand"
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
	u.setState(ops.UpdateStateDownloading, version.Version, args.Version, "")
	failed := make(chan string, 1)
	go u.runAPK(context.WithoutCancel(ctx), args.Version, args.Now, hub, failed)
	wait := u.earlyWait
	if wait == 0 {
		wait = earlyFailWait
	}
	select {
	case msg := <-failed:
		if msg != "" {
			res.Reason = msg
			return res, nil
		}
	case <-time.After(wait):
	case <-ctx.Done():
	}
	res.Accepted = true
	return res, nil
}

// setDetail changes the state and its detail, keeping from and to.
func (u *selfUpdate) setDetail(state, detail string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	at := ops.Timestamp(u.now())
	u.st.State, u.st.Error, u.st.At = state, "", &at
	u.st.Detail = proto.SafeText(detail, 300)
}

// runAPK stages the APK, waits for an idle moment and hands it to the app.
func (u *selfUpdate) runAPK(ctx context.Context, tag string, now bool, hub *version.ProtoRange, failed chan<- string) {
	a := u.apk
	held := false
	fail := func(msg string) {
		a.withdraw()
		version.RemoveStagedAPK(a.stageDir())
		if held && u.holdSessions != nil {
			u.holdSessions("")
		}
		msg = proto.SafeText(msg, 500)
		u.log.Warn("app update failed", "to", tag, "error", msg)
		u.setState(ops.UpdateStateFailed, version.Version, tag, msg)
		u.busy.Store(false)
		select {
		case failed <- msg:
		default:
		}
	}
	accepted := func() {
		select {
		case failed <- "":
		default:
		}
	}
	var curCode int64
	if st, ok := a.appState(); ok {
		curCode = st.InstalledVersionCode
	}
	sctx, cancel := context.WithTimeout(ctx, selfUpdateTimeout)
	staged, err := a.stage(sctx, version.APKOptions{Tag: tag, Current: version.Version, CurrentCode: curCode, HubRange: hub, Dir: a.stageDir()})
	cancel()
	if err != nil {
		fail(errString(err))
		return
	}
	u.log.Info("app update staged", "to", tag, "version_code", staged.VersionCode, "sha256", staged.SHA256, "base", staged.Base)
	u.setDetail(ops.UpdateStateStaged, "verified "+staged.Asset)
	accepted()

	poll := a.timing(a.poll, apkPoll)
	idleUntil := u.now().Add(a.timing(a.idleMax, apkIdleWaitMax))
	for {
		if !now {
			idle, why := a.idle(ctx)
			if !idle {
				if u.now().After(idleUntil) {
					fail("no idle moment for the update (" + why + "); use Update now")
					return
				}
				u.setDetail(ops.UpdateStateWaitingIdle, why)
				time.Sleep(poll)
				continue
			}
		}
		// No new session from here on; then look once more, so none started
		// between the check and the hold.
		if u.holdSessions != nil {
			u.holdSessions("the Minion app is updating")
			held = true
		}
		if !now {
			if idle, why := a.idle(ctx); !idle {
				if u.holdSessions != nil {
					u.holdSessions("")
					held = false
				}
				u.setDetail(ops.UpdateStateWaitingIdle, why)
				time.Sleep(poll)
				continue
			}
		}
		break
	}
	// The app's readiness can have changed while the update waited.
	if b := a.block(); b != "" {
		fail(b)
		return
	}
	id, err := newAPKRequestID()
	if err != nil {
		fail(err.Error())
		return
	}
	fromCode, _ := version.APKVersionCode(mustVersion(version.Version))
	if curCode > fromCode {
		fromCode = curCode
	}
	created := u.now()
	req := apkRequest{Schema: 1, ID: id, Tag: tag, Version: staged.Version.String(), VersionCode: staged.VersionCode,
		From: version.Version, FromVersionCode: fromCode, Package: apkPackage,
		APK: staged.Path, SHA256: staged.SHA256, Size: staged.Size, Checksums: staged.Checksums, Bundle: staged.Bundle,
		Now: now, Approved: true, CreatedAt: created, ExpiresAt: created.Add(a.timing(a.handoverTTL, apkHandoverTTL))}
	if err := a.writeRequest(req); err != nil {
		fail("cannot hand the APK to the app: " + err.Error())
		return
	}
	u.log.Info("app update handed to the app", "to", tag, "request", id, "now", now)
	u.setDetail(ops.UpdateStateInstalling, "handed to the app")

	taken := false
	installUntil := u.now().Add(a.timing(a.installMax, apkInstallMax))
	for {
		time.Sleep(poll)
		st, _ := a.appState()
		if state, reason, ok := st.attempt(id); ok {
			switch {
			case apkAppFinal[state]:
				fail("the app did not install the update (" + state + "): " + reason)
				return
			case state == apkAppNeedsConfirmation:
				taken = true
				// The install waits for a person on the device: sessions may
				// start again meanwhile.
				if held && u.holdSessions != nil {
					u.holdSessions("")
					held = false
				}
				u.setDetail(ops.UpdateStateNeedsConfirmation, "confirm the update on the device (open the Minion app)")
			case state == apkAppInstalling || state == apkAppInstalled:
				taken = true
				u.setDetail(ops.UpdateStateInstalling, "the app is installing "+staged.Asset)
			case state != "":
				// waiting_idle, waiting_throttle, waiting_approval: the app
				// holds the request and says why it waits.
				taken = true
				u.setDetail(ops.UpdateStateInstalling, "the app waits ("+state+") "+reason)
			}
		}
		switch {
		case !taken && u.now().After(req.ExpiresAt):
			fail(fmt.Sprintf("the app did not take the update within %s", a.timing(a.handoverTTL, apkHandoverTTL)))
			return
		case u.now().After(installUntil):
			fail("the app did not finish the update")
			return
		}
		if r, ok := a.readRequest(); !ok || r.ID != id {
			fail("the update request was removed")
			return
		}
	}
}

func mustVersion(s string) version.SemVer {
	v, _ := version.ParseVersion(s)
	return v
}

func newAPKRequestID() (string, error) {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", errors.New("cannot create the update request id")
	}
	return hex.EncodeToString(b[:]), nil
}

// updateIdle reports whether an app update may kill every session now: no
// client is attached to a session and no agent is busy or waits for an answer
// in one. A session host that cannot list its sessions is not idle.
func (e *Executor) updateIdle(ctx context.Context) (bool, string) {
	if e.host == nil {
		return true, ""
	}
	list, err := e.host.ListSessions(ctx)
	if err != nil {
		return false, "cannot list the sessions: " + errString(err)
	}
	for _, s := range list {
		if s.Attached {
			return false, fmt.Sprintf("a client is attached to session %q", s.Name)
		}
	}
	if e.classify == nil {
		return true, ""
	}
	for _, s := range list {
		for _, p := range s.Panes {
			scr, err := e.host.Snapshot(ctx, p.Ref)
			if err != nil {
				continue
			}
			r := e.classify.Classify(ctx, agentdetect.Input{Command: p.Ref.Command, PID: p.Ref.PID, Screen: scr})
			switch r.State {
			case ops.PaneStateBusy:
				return false, fmt.Sprintf("an agent is working in session %q", s.Name)
			case ops.PaneStateDialog:
				return false, fmt.Sprintf("an agent waits for an answer in session %q", s.Name)
			}
		}
	}
	return true, ""
}
