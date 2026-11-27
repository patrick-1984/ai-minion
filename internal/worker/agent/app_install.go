package agent

// App install on the Android Worker (v0.25.0, docs/guides/android-apps.md),
// Worker side. The Worker never installs anything itself: it checks the APK
// that file transfer put into the Minion files folder (the confinement of
// internal/filexfer: a relative path, no symbolic link on the way, a regular
// file, at most ops.AppInstallMaxBytes), computes its SHA-256 and hands a
// request to the app (AppInstaller.kt), which checks the archive (package,
// version, signer, not Minion itself), streams it into a PackageInstaller
// session while hashing it against the digest of the request, and reports the
// outcome.
//
// Handover (files in <state>/update, the app's private storage; every write is
// a write of a temporary file and a rename), next to the self-update files:
//
//	app-install-pending.json   Worker -> app: "install this", see appInstallRequest.
//	                           The Worker removes it to withdraw the request.
//	app-install-status.json    app -> Worker: readiness and the attempt of the
//	                           request it took, see appInstallAppState.
//
// Every process of the app's user can rewrite those files: the app does not
// trust the request's facts about the archive, it reads them from the archive
// itself, requires the streamed bytes to have the request's digest, and the
// Hub decides who may ask at all.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/patrick-1984/ai-minion/internal/filexfer"
	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
)

// Names of the app install handover files in <state>/update.
const (
	appInstallRequestFile = "app-install-pending.json"
	appInstallAppFile     = "app-install-status.json"
)

// Timings of the app install handover.
const (
	// appInstallPoll is how often the Worker reads the app's answer.
	appInstallPoll = 500 * time.Millisecond
	// appInstallTTL is how long the app may take to take a request (it reads
	// it on the Worker service tick).
	appInstallTTL = 60 * time.Second
	// appInstallWait bounds how long app_install waits for an outcome before
	// it answers installing (the Hub waits two minutes for an answer).
	appInstallWait = 75 * time.Second
)

// appInstallRequest is app-install-pending.json.
type appInstallRequest struct {
	Schema int    `json:"schema"`
	ID     string `json:"id"`
	// Path is the absolute path of the APK in the Minion files folder, RelPath
	// the path relative to it (the argument).
	Path      string    `json:"path"`
	RelPath   string    `json:"rel_path"`
	SHA256    string    `json:"sha256"`
	Size      int64     `json:"size"`
	AllowNew  bool      `json:"allow_new"`
	CreatedAt time.Time `json:"created_at"`
	ExpiresAt time.Time `json:"expires_at"`
}

// appInstallAppState is app-install-status.json, written by the app.
type appInstallAppState struct {
	Schema      int                   `json:"schema"`
	Ready       bool                  `json:"ready"`
	BlockReason string                `json:"block_reason,omitempty"`
	Attempt     *appInstallAppAttempt `json:"attempt,omitempty"`
}

// appInstallAppAttempt is the attempt object of app-install-status.json.
type appInstallAppAttempt struct {
	ID                   string `json:"id"`
	State                string `json:"state"`
	Reason               string `json:"reason,omitempty"`
	Package              string `json:"package,omitempty"`
	VersionCode          int64  `json:"version_code,omitempty"`
	VersionName          string `json:"version_name,omitempty"`
	Label                string `json:"label,omitempty"`
	SignerSHA256         string `json:"signer_sha256,omitempty"`
	InstalledVersionCode int64  `json:"installed_version_code,omitempty"`
	NewInstall           bool   `json:"new_install,omitempty"`
}

// Attempt states of app-install-status.json (AppInstaller.kt).
const (
	appStateChecking     = "checking"
	appStateInstalling   = "installing"
	appStateConfirmation = "needs_confirmation_on_device"
	appStateInstalled    = "installed"
	appStateRefused      = "refused"
	appStateFailed       = "failed"
	appStateDeclined     = "declined"
)

// appInstaller is the Worker side of app_install and app_install_status.
type appInstaller struct {
	// dir is <state>/update, confine the Minion files folder.
	dir, confine string
	// floor is the identity floor of the file transfer roots (nil: none, tests).
	floor func() *filexfer.Floor
	now   func() time.Time
	// poll, ttl and wait override the timings (tests).
	poll, ttl, wait time.Duration
	// mu serializes the requests: one install at a time.
	mu sync.Mutex
}

// AttachAppInstall registers app_install and app_install_status with the
// executor and announces proto.FeatureAppInstallV1. The Android app build
// calls it (its app reads the requests); stateDir is the Worker's state
// folder and confine the Minion files folder.
func (e *Executor) AttachAppInstall(stateDir, confine string, floor func() *filexfer.Floor) {
	a := &appInstaller{dir: filepath.Join(stateDir, apkUpdateDir), confine: confine, floor: floor}
	e.appInstall.Store(a)
	e.Handle(ops.OpAppInstall, a.handleInstall)
	e.Handle(ops.OpAppInstallStatus, a.handleStatus)
}

func (a *appInstaller) timing(d, def time.Duration) time.Duration {
	if d > 0 {
		return d
	}
	return def
}

func (a *appInstaller) clock() time.Time {
	if a.now != nil {
		return a.now()
	}
	return time.Now()
}

func (a *appInstaller) openRoot() (*filexfer.Root, error) {
	var fl *filexfer.Floor
	if a.floor != nil {
		fl = a.floor()
	}
	return filexfer.OpenDirRoot(a.confine, filexfer.RootOptions{Floor: fl, Confine: a.confine, ConfineLabel: ConfineLabelAndroid})
}

// appState reads app-install-status.json; ok is false when the app has not
// written it.
func (a *appInstaller) appState() (appInstallAppState, bool) {
	b, err := os.ReadFile(filepath.Join(a.dir, appInstallAppFile))
	if err != nil || len(b) > 64<<10 {
		return appInstallAppState{}, false
	}
	var st appInstallAppState
	if json.Unmarshal(b, &st) != nil || st.Schema < 1 {
		return appInstallAppState{}, false
	}
	return st, true
}

func (a *appInstaller) readRequest() (appInstallRequest, bool) {
	b, err := os.ReadFile(filepath.Join(a.dir, appInstallRequestFile))
	if err != nil || len(b) > 64<<10 {
		return appInstallRequest{}, false
	}
	var r appInstallRequest
	return r, json.Unmarshal(b, &r) == nil
}

func (a *appInstaller) writeRequest(r appInstallRequest) error {
	if err := os.MkdirAll(a.dir, 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	tmp := filepath.Join(a.dir, "."+appInstallRequestFile+".tmp")
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(a.dir, appInstallRequestFile))
}

// withdraw removes the request id (and only that one).
func (a *appInstaller) withdraw(id string) {
	if r, ok := a.readRequest(); ok && r.ID == id {
		_ = os.Remove(filepath.Join(a.dir, appInstallRequestFile))
	}
}

// inspect opens rel inside the Minion files folder and returns its size and
// SHA-256: a regular file reached without a symbolic link, at most
// ops.AppInstallMaxBytes.
func (a *appInstaller) inspect(ctx context.Context, rel string) (size int64, digest string, err error) {
	root, err := a.openRoot()
	if err != nil {
		return 0, "", fileErr(err, "the Minion files folder cannot be opened")
	}
	defer root.Close()
	f, err := root.OpenFile(rel)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return 0, "", apiErr(proto.CodeNotFound, "", "no such file in the Minion files folder: "+proto.SafeText(rel, 200))
		}
		return 0, "", fileErr(err, "the file cannot be opened")
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return 0, "", internalErr("stat", err)
	}
	if !fi.Mode().IsRegular() {
		return 0, "", apiErr(proto.CodeFileTypeRefused, "", "the path is not a regular file")
	}
	if fi.Size() <= 0 {
		return 0, "", apiErr(proto.CodeInvalidArgument, "path", "the file is empty")
	}
	if fi.Size() > ops.AppInstallMaxBytes {
		return 0, "", apiErr(proto.CodeTransferTooLarge, "", "the APK is larger than 512 MiB")
	}
	h := sha256.New()
	n, err := io.Copy(h, io.LimitReader(ctxReader{ctx, f}, ops.AppInstallMaxBytes+1))
	if err != nil {
		return 0, "", fileErr(err, "the file cannot be read")
	}
	if n != fi.Size() {
		return 0, "", apiErr(proto.CodeConflict, "", "the file changed while it was read")
	}
	return n, hex.EncodeToString(h.Sum(nil)), nil
}

// fileErr maps a confinement refusal to its proto error, any other error to
// an internal one.
func fileErr(err error, what string) error {
	var r *filexfer.Refusal
	if errors.As(err, &r) {
		return apiErr(r.Code, "", r.Msg)
	}
	return internalErr(what, err)
}

// ctxReader stops a long read when the call ends.
type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (c ctxReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}

// appFromHub requires a request through the Hub, which decides and audits
// every install (a local caller is refused), for this Worker.
func appFromHub(rq Request, workerID string) error {
	if rq.Origin == nil {
		return apiErr(proto.CodeForbidden, "", "app installs run through the Hub, which authorizes and audits them: use minion apps install with a Hub")
	}
	if rq.TargetWorkerID != "" && workerID != "" && workerID != rq.TargetWorkerID {
		return apiErr(proto.CodeForbidden, "", "worker_id names another Worker")
	}
	return nil
}

// handleInstall is app_install.
func (a *appInstaller) handleInstall(ctx context.Context, rq Request) (any, error) {
	_, args, err := prepare[ops.AppInstallArgs](rq)
	if err != nil {
		return nil, err
	}
	if err := appFromHub(rq, args.WorkerID); err != nil {
		return nil, err
	}
	if !a.mu.TryLock() {
		return ops.AppInstallResult{Outcome: ops.AppInstallRefused, Path: args.Path, Reason: "another app install is running on this Worker"}, nil
	}
	defer a.mu.Unlock()
	res := ops.AppInstallResult{Path: args.Path}
	st, ok := a.appState()
	switch {
	case !ok:
		res.Outcome, res.Reason = ops.AppInstallRefused, "the Minion app has not reported that it can install apps yet; open the app once"
		return res, nil
	case !st.Ready:
		res.Outcome, res.Reason = ops.AppInstallRefused, "the Minion app cannot install apps: "+proto.SafeText(orText(st.BlockReason, "not ready"), 300)
		return res, nil
	}
	if r, ok := a.readRequest(); ok && a.clock().Before(r.ExpiresAt) {
		if s, ok := a.appState(); !ok || s.Attempt == nil || s.Attempt.ID != r.ID || !appFinal(s.Attempt.State) {
			res.Outcome, res.Reason = ops.AppInstallRefused, "another app install is waiting for the app"
			return res, nil
		}
	}
	size, digest, err := a.inspect(ctx, args.Path)
	if err != nil {
		return nil, err
	}
	res.Size, res.SHA256 = size, digest
	id, err := newAPKRequestID()
	if err != nil {
		return nil, internalErr("request id", err)
	}
	res.RequestID = id
	created := a.clock()
	req := appInstallRequest{Schema: 1, ID: id, Path: filepath.Join(a.confine, filepath.FromSlash(args.Path)), RelPath: args.Path,
		SHA256: digest, Size: size, AllowNew: args.AllowNew, CreatedAt: created.UTC(),
		ExpiresAt: created.Add(a.timing(a.ttl, appInstallTTL)).UTC()}
	if err := a.writeRequest(req); err != nil {
		return nil, internalErr("hand the APK to the app", err)
	}
	return a.await(ctx, req, res), nil
}

// await waits for the outcome of req: a final state, a confirmation on the
// device, or installing when the wait ends first.
func (a *appInstaller) await(ctx context.Context, req appInstallRequest, res ops.AppInstallResult) ops.AppInstallResult {
	poll := a.timing(a.poll, appInstallPoll)
	until := a.clock().Add(a.timing(a.wait, appInstallWait))
	taken := false
	t := time.NewTicker(poll)
	defer t.Stop()
	for {
		if st, ok := a.appState(); ok && st.Attempt != nil && st.Attempt.ID == req.ID {
			taken = true
			out := attemptResult(*st.Attempt, res)
			switch {
			case appFinal(st.Attempt.State):
				a.withdraw(req.ID)
				return out
			case st.Attempt.State == appStateConfirmation:
				// The request stays until the person decides on the device.
				return out
			}
			res = out
		}
		if !taken && a.clock().After(req.ExpiresAt) {
			a.withdraw(req.ID)
			res.Outcome, res.Reason = ops.AppInstallFailed, "the Minion app did not take the request in time; is the app running?"
			return res
		}
		if a.clock().After(until) {
			res.Outcome, res.Reason = ops.AppInstallInstalling, "the app is still installing; see app_install_status (minion apps status)"
			return res
		}
		select {
		case <-ctx.Done():
			if !taken {
				a.withdraw(req.ID)
				res.Outcome, res.Reason = ops.AppInstallFailed, "the request was cancelled"
				return res
			}
			res.Outcome, res.Reason = ops.AppInstallInstalling, "the app is still installing; see app_install_status (minion apps status)"
			return res
		case <-t.C:
		}
	}
}

func appFinal(state string) bool {
	switch state {
	case appStateInstalled, appStateRefused, appStateFailed, appStateDeclined:
		return true
	}
	return false
}

// attemptResult maps the app's attempt onto the result (keeping the Worker's
// own path, digest and size).
func attemptResult(at appInstallAppAttempt, base ops.AppInstallResult) ops.AppInstallResult {
	out := base
	out.RequestID = at.ID
	out.Package = proto.SafeText(at.Package, 255)
	out.VersionCode, out.InstalledVersionCode, out.NewInstall = at.VersionCode, at.InstalledVersionCode, at.NewInstall
	out.VersionName = proto.SafeText(at.VersionName, 100)
	out.Label = proto.SafeText(at.Label, 100)
	out.SignerSHA256 = strings.ToLower(proto.SafeText(at.SignerSHA256, 64))
	out.Reason = proto.SafeText(at.Reason, 300)
	switch at.State {
	case appStateInstalled:
		out.Outcome = ops.AppInstallInstalled
	case appStateConfirmation:
		out.Outcome = ops.AppInstallNeedsConfirmation
		if out.Reason == "" {
			out.Reason = "confirm the install on the device: open Minion and choose Install"
		}
	case appStateRefused:
		out.Outcome = ops.AppInstallRefused
	case appStateDeclined:
		out.Outcome = ops.AppInstallRefused
		if out.Reason == "" {
			out.Reason = "the install was cancelled on the device"
		}
	case appStateFailed:
		out.Outcome = ops.AppInstallFailed
	default:
		out.Outcome = ops.AppInstallInstalling
	}
	return out
}

// handleStatus is app_install_status.
func (a *appInstaller) handleStatus(_ context.Context, rq Request) (any, error) {
	if _, _, err := prepare[ops.AppInstallStatusArgs](rq); err != nil {
		return nil, err
	}
	out := ops.AppInstallStatus{Files: []ops.AppInstallFile{}}
	st, ok := a.appState()
	switch {
	case !ok:
		out.BlockReason = "the Minion app has not reported that it can install apps yet; open the app once"
	case !st.Ready:
		out.BlockReason = proto.SafeText(orText(st.BlockReason, "the Minion app cannot install apps now"), 300)
	default:
		out.Ready = true
	}
	if ok && st.Attempt != nil {
		base := ops.AppInstallResult{}
		if r, ok := a.readRequest(); ok && r.ID == st.Attempt.ID {
			base.Path, base.SHA256, base.Size = r.RelPath, r.SHA256, r.Size
		}
		last := attemptResult(*st.Attempt, base)
		out.Last = &last
	}
	out.Files = a.listAPKs()
	return out, nil
}

// listAPKs lists the regular .apk files at the top of the Minion files folder.
func (a *appInstaller) listAPKs() []ops.AppInstallFile {
	out := []ops.AppInstallFile{}
	root, err := a.openRoot()
	if err != nil {
		return out
	}
	defer root.Close()
	d, err := root.OpenDir("")
	if err != nil {
		return out
	}
	defer d.Close()
	entries, err := d.ReadDir(-1)
	if err != nil {
		return out
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	for _, e := range entries {
		name := e.Name()
		if !e.Type().IsRegular() || strings.HasPrefix(name, ".") || !strings.EqualFold(path.Ext(name), ".apk") {
			continue
		}
		if ops.ValidAppInstallPath(name) != nil {
			continue
		}
		// The size from the entry opened inside the root (Info would stat
		// by a path).
		f, err := root.OpenFile(name)
		if err != nil {
			continue
		}
		fi, err := f.Stat()
		_ = f.Close()
		if err != nil || !fi.Mode().IsRegular() {
			continue
		}
		out = append(out, ops.AppInstallFile{Path: name, Size: fi.Size()})
		if len(out) >= ops.AppInstallMaxListed {
			break
		}
	}
	return out
}
