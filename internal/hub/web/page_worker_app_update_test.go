package web

import (
	"net/url"
	"strings"
	"testing"

	"github.com/patrick-1984/ai-minion/internal/ops"
)

// An Android Worker offers Update (when idle) and Update now, both warning
// that its sessions restart and kept sessions come back; a Worker whose
// updates from the Hub are off offers no Update and says why.
func TestAndroidWorkerUpdateWarnsAboutSessions(t *testing.T) {
	withHubVersion(t, "v0.24.0")
	tv := wOld
	tv.ID, tv.Name, tv.Build, tv.Android = "w1", "tv", "v0.23.0", true
	off := tv
	off.ID, off.Name, off.AppUpdate = "w2", "phone", ops.WorkerAppUpdateOff
	admin := Dashboard{Admin: true, Workers: []WorkerRow{tv, off}}
	s, _ := listServer(t, Dashboard{}, admin)
	body := get(t, s, "/workers?scope=all", "X-Test-Session", adminHeader).Body.String()
	mustHave(t, body, `action="/workers/w1/update"`, `data-control="update-now"`, `sessions will restart; kept sessions come back`,
		`(updates from the Hub are off for this worker)`)
	mustNotHave(t, body, `action="/workers/w2/update"`)
}

// Update now sends now=true.
func TestAndroidUpdateNowSendsNow(t *testing.T) {
	withHubVersion(t, "v0.24.0")
	f := newFakeOps().reply(ops.OpWorkerUpdate, ops.WorkerUpdateResult{Accepted: true, From: "v0.23.0", To: "v0.24.0"})
	connect(t, f)
	rec := post(t, adminServer(t), "/workers/w1/update", adminHeader, url.Values{"android": {"1"}, "now": {"1"}})
	if rec.Code != 200 {
		t.Fatalf("POST = %d %s", rec.Code, rec.Body.String())
	}
	calls := f.called(ops.OpWorkerUpdate)
	if len(calls) != 1 || calls[0].Args["now"] != true {
		t.Fatalf("calls = %+v", calls)
	}
}

// The states of an Android app update are progress, not failures.
func TestUpdateStatusTextAndroidStates(t *testing.T) {
	const to = "v0.24.0"
	for state, want := range map[string]string{
		ops.UpdateStateDownloading:       "downloading and verifying",
		ops.UpdateStateStaged:            "verified (verified minion-android-0.24.0.apk)",
		ops.UpdateStateWaitingIdle:       "waiting until no client is attached and no agent is busy (verified minion-android-0.24.0.apk)",
		ops.UpdateStateInstalling:        "installing on the device",
		ops.UpdateStateNeedsConfirmation: "needs confirmation on the device",
	} {
		got := updateStatusText(ops.WorkerUpdateStatus{State: state, To: to, Detail: "verified minion-android-0.24.0.apk"}, to)
		if !isUpdateProgress(got) || !strings.Contains(got, want) {
			t.Errorf("%s: %q", state, got)
		}
	}
	got := updateStatusText(ops.WorkerUpdateStatus{State: ops.UpdateStateFailed, To: to, Error: "the app did not install the update (declined): "}, to)
	if isUpdateProgress(got) || !strings.HasPrefix(got, "Update to v0.24.0 failed") {
		t.Errorf("failed: %q", got)
	}
	if got := updateStatusText(ops.WorkerUpdateStatus{State: ops.UpdateStateInstalling, To: "v0.23.5"}, to); got != "" {
		t.Errorf("other target: %q", got)
	}
}
