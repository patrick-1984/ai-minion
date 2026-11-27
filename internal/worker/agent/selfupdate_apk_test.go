package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/version"
)

// fakeApp plays the Kotlin updater: it writes apk-app.json.
type fakeApp struct {
	t   *testing.T
	dir string
}

func (f fakeApp) write(st apkAppState) {
	f.t.Helper()
	st.Schema = 1
	b, _ := json.Marshal(st)
	if err := os.WriteFile(filepath.Join(f.dir, apkAppFile), b, 0o600); err != nil {
		f.t.Fatal(err)
	}
}

type apkFixture struct {
	u        *selfUpdate
	app      fakeApp
	idle     atomic.Value // string: "" idle, else why not
	holds    []string
	mu       sync.Mutex
	staged   []version.APKOptions
	stageErr error
}

func newAPKFixture(t *testing.T) *apkFixture {
	t.Helper()
	withVersion(t, "v0.23.0")
	u, _ := newTestUpdate(t, &fakeInstaller{})
	f := &apkFixture{u: u}
	f.idle.Store("")
	dir := filepath.Join(u.stateDir, apkUpdateDir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	f.app = fakeApp{t: t, dir: dir}
	u.holdSessions = func(r string) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.holds = append(f.holds, r)
	}
	u.apk = &apkUpdater{
		dir: dir,
		stage: func(_ context.Context, o version.APKOptions) (version.StagedAPK, error) {
			f.mu.Lock()
			f.staged = append(f.staged, o)
			err := f.stageErr
			f.mu.Unlock()
			if err != nil {
				return version.StagedAPK{}, err
			}
			v, _, _ := version.ParseReleaseTag(o.Tag)
			code, _ := version.APKVersionCode(v)
			p := filepath.Join(o.Dir, version.APKAssetName(v))
			_ = os.MkdirAll(o.Dir, 0o700)
			_ = os.WriteFile(p, []byte("apk"), 0o400)
			return version.StagedAPK{Tag: o.Tag, Version: v, VersionCode: code, Asset: filepath.Base(p), SHA256: strings.Repeat("a", 64), Size: 3, Path: p}, nil
		},
		idle: func(context.Context) (bool, string) {
			why := f.idle.Load().(string)
			return why == "", why
		},
		poll: 5 * time.Millisecond, handoverTTL: 300 * time.Millisecond,
	}
	f.app.write(apkAppState{Ready: true, InstalledVersionCode: 2_300_999})
	// An update a test leaves running (for example one that waits for the
	// confirmation on the device) ends here, before withVersion restores
	// version.Version, which the update goroutine reads.
	t.Cleanup(func() {
		deadline := time.Now().Add(10 * time.Second)
		for u.busy.Load() && time.Now().Before(deadline) {
			u.apk.withdraw()
			time.Sleep(2 * time.Millisecond)
		}
		if u.busy.Load() {
			t.Error("the update goroutine did not end")
		}
	})
	return f
}

func (f *apkFixture) waitState(t *testing.T, state string) ops.WorkerUpdateStatus {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if st := f.u.status(); st.State == state {
			return st
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("state %q never reached; now %+v", state, f.u.status())
	return ops.WorkerUpdateStatus{}
}

func (f *apkFixture) request(t *testing.T) apkRequest {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if r, ok := f.u.apk.readRequest(); ok {
			return r
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("no request")
	return apkRequest{}
}

func TestAPKUpdateWaitsForIdleAndHandsOver(t *testing.T) {
	f := newAPKFixture(t)
	f.idle.Store(`an agent is working in session "a"`)
	res, err := callUpdate(t, f.u, ops.WorkerUpdateArgs{Version: "v0.24.0"})
	if err != nil || !res.Accepted {
		t.Fatalf("res = %+v, %v", res, err)
	}
	st := f.waitState(t, ops.UpdateStateWaitingIdle)
	if !strings.Contains(st.Detail, "agent is working") || st.To != "v0.24.0" {
		t.Errorf("waiting = %+v", st)
	}
	if _, ok := f.u.apk.readRequest(); ok {
		t.Fatal("handed over while busy")
	}
	f.idle.Store("")
	req := f.request(t)
	if req.Tag != "v0.24.0" || req.VersionCode != 2_400_999 || req.FromVersionCode != 2_300_999 || req.Package != "cc.nasz.minion" ||
		req.SHA256 != strings.Repeat("a", 64) || req.Now || !req.Approved || req.ID == "" || !req.ExpiresAt.After(req.CreatedAt) {
		t.Errorf("request = %+v", req)
	}
	f.mu.Lock()
	if len(f.staged) != 1 || f.staged[0].CurrentCode != 2_300_999 || f.staged[0].HubRange == nil {
		t.Errorf("stage options = %+v", f.staged)
	}
	if len(f.holds) == 0 || f.holds[len(f.holds)-1] == "" {
		t.Errorf("sessions not held at the handover: %q", f.holds)
	}
	f.mu.Unlock()
	f.waitState(t, ops.UpdateStateInstalling)
	// The app reports a failure: the Worker withdraws, cleans up and frees.
	f.app.write(apkAppState{Ready: true, Attempt: &apkAppAttempt{ID: req.ID, State: apkAppFailed, Reason: "INSTALL_FAILED_UPDATE_INCOMPATIBLE"}})
	st = f.waitState(t, ops.UpdateStateFailed)
	if !strings.Contains(st.Error, "INSTALL_FAILED_UPDATE_INCOMPATIBLE") {
		t.Errorf("failed = %+v", st)
	}
	if _, ok := f.u.apk.readRequest(); ok {
		t.Error("request kept after the failure")
	}
	if _, err := os.Stat(req.APK); !os.IsNotExist(err) {
		t.Errorf("staged APK kept: %v", err)
	}
	f.mu.Lock()
	if f.holds[len(f.holds)-1] != "" {
		t.Errorf("session hold not released: %q", f.holds)
	}
	f.mu.Unlock()
	if f.u.busy.Load() {
		t.Error("still busy")
	}
}

func TestAPKUpdateNowSkipsTheIdleWait(t *testing.T) {
	f := newAPKFixture(t)
	f.idle.Store(`a client is attached to session "a"`)
	if res, err := callUpdate(t, f.u, ops.WorkerUpdateArgs{Version: "android-v0.23.1", Now: true}); err != nil || !res.Accepted {
		t.Fatalf("res = %+v, %v", res, err)
	}
	req := f.request(t)
	if !req.Now || req.Tag != "android-v0.23.1" || req.Version != "v0.23.1" || req.VersionCode != 2_301_999 {
		t.Errorf("request = %+v", req)
	}
	f.app.write(apkAppState{Ready: true, Attempt: &apkAppAttempt{ID: req.ID, State: apkAppNeedsConfirmation}})
	st := f.waitState(t, ops.UpdateStateNeedsConfirmation)
	if !strings.Contains(st.Detail, "device") {
		t.Errorf("needs confirmation = %+v", st)
	}
	// Taken by the app: the handover deadline no longer applies.
	time.Sleep(400 * time.Millisecond)
	if st := f.u.status(); st.State != ops.UpdateStateNeedsConfirmation {
		t.Errorf("after the handover deadline: %+v", st)
	}
}

func TestAPKUpdateNotTakenByTheAppIsWithdrawn(t *testing.T) {
	f := newAPKFixture(t)
	if res, err := callUpdate(t, f.u, ops.WorkerUpdateArgs{Version: "v0.24.0"}); err != nil || !res.Accepted {
		t.Fatalf("res = %+v, %v", res, err)
	}
	f.request(t)
	st := f.waitState(t, ops.UpdateStateFailed)
	if !strings.Contains(st.Error, "did not take") {
		t.Errorf("failed = %+v", st)
	}
	if _, ok := f.u.apk.readRequest(); ok {
		t.Error("request not withdrawn")
	}
}

func TestAPKUpdateRefusals(t *testing.T) {
	f := newAPKFixture(t)
	for _, c := range []struct {
		version, want string
	}{
		{"v0.22.0", "downgrade"},
		{"android-v0.22.9", "downgrade"},
		{"v0.23.0", "already on"},
		{"android-v0.23.0", "already on"},
	} {
		res, err := callUpdate(t, f.u, ops.WorkerUpdateArgs{Version: c.version})
		if err != nil || res.Accepted || !strings.Contains(res.Reason, c.want) {
			t.Errorf("%s: %+v, %v", c.version, res, err)
		}
	}
	for _, bad := range []string{"latest", "android-v0.24.0-rc.1", "0.24.0"} {
		if _, err := callUpdate(t, f.u, ops.WorkerUpdateArgs{Version: bad}); err == nil {
			t.Errorf("%s accepted", bad)
		}
	}
	// The app says it cannot install: the reason reaches the Hub.
	f.app.write(apkAppState{Ready: false, Reason: "Install unknown apps is not allowed for Minion"})
	res, _ := callUpdate(t, f.u, ops.WorkerUpdateArgs{Version: "v0.24.0"})
	if res.Accepted || !strings.Contains(res.Reason, "Install unknown apps") {
		t.Errorf("not ready: %+v", res)
	}
	if got := f.u.blockReason(); !strings.Contains(got, "Install unknown apps") {
		t.Errorf("block = %q", got)
	}
	_ = os.Remove(filepath.Join(f.u.apk.dir, apkAppFile))
	if got := f.u.blockReason(); !strings.Contains(got, "has not reported") {
		t.Errorf("no app state: %q", got)
	}
	// A verification failure is the answer.
	f.app.write(apkAppState{Ready: true})
	f.stageErr = version.ErrChecksum
	res, _ = callUpdate(t, f.u, ops.WorkerUpdateArgs{Version: "v0.24.0"})
	if res.Accepted || !strings.Contains(res.Reason, "checksum") {
		t.Errorf("stage failure: %+v", res)
	}
}

func TestAPKStartUpWithdrawsAnOldRequest(t *testing.T) {
	f := newAPKFixture(t)
	a := f.u.apk
	stage := a.stageDir()
	_ = os.MkdirAll(stage, 0o700)
	_ = os.WriteFile(filepath.Join(stage, "minion-android-0.24.0.apk"), []byte("x"), 0o400)
	if err := a.writeRequest(apkRequest{Schema: 1, ID: "r1", Tag: "v0.24.0", From: "v0.23.0"}); err != nil {
		t.Fatal(err)
	}
	f.app.write(apkAppState{Ready: true, Attempt: &apkAppAttempt{ID: "r1", State: "refused", Reason: "INSTALL_FAILED_INSUFFICIENT_STORAGE"}})
	a.startUp(f.u)
	if _, ok := a.readRequest(); ok {
		t.Error("old request kept")
	}
	if st := f.u.status(); st.State != ops.UpdateStateFailed || !strings.Contains(st.Error, "INSUFFICIENT_STORAGE") {
		t.Errorf("status = %+v", st)
	}
	if ents, _ := os.ReadDir(stage); len(ents) != 0 {
		t.Errorf("staged files kept: %v", ents)
	}
}

func TestAndroidUpdateBlock(t *testing.T) {
	withVersion(t, "v0.24.0")
	if got := androidUpdateBlock("off", "/s"); !strings.Contains(got, "remote_update=off") {
		t.Errorf("off: %q", got)
	}
	if got := androidUpdateBlock("on", "/s"); got != "" {
		t.Errorf("on: %q", got)
	}
	withVersion(t, "dev")
	if got := androidUpdateBlock("on", "/s"); !strings.Contains(got, "development build") {
		t.Errorf("dev: %q", got)
	}
}

// The idle decision comes from Minion's own session state: an attached client
// or a busy or asking agent in any pane holds the update.
func TestUpdateIdleFromSessionState(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	if idle, why := e.ex.updateIdle(ctx); !idle {
		t.Fatalf("no sessions: %s", why)
	}
	sh := e.host.addSession("shell", "bash")
	e.host.setScreen(sh, "$")
	if idle, why := e.ex.updateIdle(ctx); !idle {
		t.Fatalf("a quiet shell: %s", why)
	}
	ag := e.host.addSession("agent", "claude")
	e.host.setScreen(ag, "working")
	e.cls.set("claude", ops.PaneKindAgentPrefix+"claude", ops.PaneStateBusy)
	if idle, why := e.ex.updateIdle(ctx); idle || !strings.Contains(why, `working in session "agent"`) {
		t.Errorf("busy agent: %v %s", idle, why)
	}
	e.cls.set("claude", ops.PaneKindAgentPrefix+"claude", ops.PaneStateDialog)
	if idle, why := e.ex.updateIdle(ctx); idle || !strings.Contains(why, "waits for an answer") {
		t.Errorf("asking agent: %v %s", idle, why)
	}
	e.cls.set("claude", ops.PaneKindAgentPrefix+"claude", ops.PaneStateStable)
	if idle, why := e.ex.updateIdle(ctx); !idle {
		t.Errorf("stable agent: %s", why)
	}
	e.host.mu.Lock()
	e.host.sessions[0].Attached = true
	e.host.mu.Unlock()
	if idle, why := e.ex.updateIdle(ctx); idle || !strings.Contains(why, `attached to session "shell"`) {
		t.Errorf("attached: %v %s", idle, why)
	}
}
