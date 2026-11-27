package web

import (
	"net/url"
	"strings"
	"testing"

	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
)

// The app install panel: an Android Worker with app.install.v1 lists the APKs
// of its Minion files folder and the last install; Install sends app_install
// with the path and allow_new for the owner only and shows the outcome; a
// Worker without the feature shows nothing.
func TestWorkerAppsPanel(t *testing.T) {
	d := Dashboard{Workers: []WorkerRow{wPrivate}}
	w := wAliceInfo
	w.OS, w.Online = "android", true
	w.Features = append(w.Features, proto.FeatureAppInstallV1)
	w.Ops = append(w.Ops, ops.OpAppInstall, ops.OpAppInstallStatus)
	bob := wBobInfo
	bob.OS, bob.Online, bob.Features, bob.Ops = "android", true, w.Features, w.Ops
	signer := strings.Repeat("ab", 32)
	status := ops.AppInstallStatus{Ready: true, Files: []ops.AppInstallFile{{Path: "player.apk", Size: 1234}},
		Last: &ops.AppInstallResult{Outcome: ops.AppInstallInstalled, Label: "Player", Package: "org.example.player",
			VersionName: "2.1", VersionCode: 21, SignerSHA256: signer, Path: "player.apk"}}
	install := ops.AppInstallResult{Outcome: ops.AppInstallNeedsConfirmation, Label: "Player", Path: "player.apk"}
	f := newFakeOps().
		reply(ops.OpWorkersList, ops.WorkersListResult{Workers: []ops.WorkerInfo{w, bob}}).
		reply(ops.OpAppInstallStatus, status).
		on(ops.OpAppInstall, func(map[string]any) (any, error) { return install, nil })
	connect(t, f)
	s, _ := listServer(t, d, d)

	body := get(t, s, "/w/w1/apps", "X-Test-Session", "alice:user").Body.String()
	mustHave(t, body, `id="apps-section"`, "Install an app from Minion files", "player.apk", "1234 bytes", `name="path" value="player.apk"`,
		`name="allow_new" id="apps-allow-new" value="1"`, "Player is installed.", "org.example.player", "2.1", "(code 21)", signer,
		`hx-post="/w/w1/apps"`)
	if len(f.called(ops.OpAppInstall)) != 0 {
		t.Fatal("a GET installed an app")
	}

	rec := post(t, s, "/w/w1/apps", "alice:user", url.Values{"path": {"player.apk"}, "allow_new": {"1"}})
	mustHave(t, rec.Body.String(), "press Install Player on the Minion screen of the device")
	calls := f.called(ops.OpAppInstall)
	if len(calls) != 1 || calls[0].Args["worker_id"] != w.ID || calls[0].Args["path"] != "player.apk" || calls[0].Args["allow_new"] != true {
		t.Fatalf("calls = %+v", calls)
	}
	// Without the box allow_new is not sent.
	install = ops.AppInstallResult{Outcome: ops.AppInstallRefused, Reason: "not installed on this device; pass --allow-new to install a new app"}
	rec = post(t, s, "/w/w1/apps", "alice:user", url.Values{"path": {"player.apk"}})
	mustHave(t, rec.Body.String(), "Refused: not installed on this device")
	if calls := f.called(ops.OpAppInstall); len(calls) != 2 || calls[1].Args["allow_new"] != nil {
		t.Fatalf("calls = %+v", calls)
	}
	// A path that is not an APK of the folder is not sent.
	rec = post(t, s, "/w/w1/apps", "alice:user", url.Values{"path": {"../x.apk"}})
	mustHave(t, rec.Body.String(), "Choose an .apk file")
	// A refusal of the Hub is shown.
	f.on(ops.OpAppInstall, func(map[string]any) (any, error) {
		return nil, &OpError{Status: 403, Code: "forbidden", Message: "not allowed"}
	})
	rec = post(t, s, "/w/w1/apps", "alice:user", url.Values{"path": {"player.apk"}})
	mustHave(t, rec.Body.String(), "not allowed")
	if n := len(f.called(ops.OpAppInstall)); n != 3 {
		t.Fatalf("app_install calls = %d, want 3", n)
	}

	// Bob's Worker: alice may not install; nothing is called.
	post(t, s, "/w/w2/apps", "alice:user", url.Values{"path": {"player.apk"}})
	if n := len(f.called(ops.OpAppInstall)); n != 3 {
		t.Fatal("app_install was called for a person who may not operate the worker")
	}

	// A Worker without the feature: the panel renders nothing.
	w.Features = nil
	f.reply(ops.OpWorkersList, ops.WorkersListResult{Workers: []ops.WorkerInfo{w, bob}})
	if body := get(t, s, "/w/w1/apps", "X-Test-Session", "alice:user").Body.String(); strings.TrimSpace(body) != "" {
		t.Fatalf("a worker without app.install.v1 shows the panel: %q", body)
	}
}

func TestAppOutcomeTexts(t *testing.T) {
	for _, tc := range []struct {
		r    ops.AppInstallResult
		want string
	}{
		{ops.AppInstallResult{Outcome: ops.AppInstallInstalled, Package: "org.example"}, "org.example is installed."},
		{ops.AppInstallResult{Outcome: ops.AppInstallInstalling, Path: "a.apk"}, "a.apk is being installed"},
		{ops.AppInstallResult{Outcome: ops.AppInstallFailed, Reason: "not enough storage"}, "Failed: not enough storage"},
		{ops.AppInstallResult{Outcome: ops.AppInstallNeedsConfirmation, Label: "TV"}, "press Install TV"},
	} {
		if got := appOutcome(tc.r).Text; !strings.Contains(got, tc.want) {
			t.Errorf("%s: %q, want %q", tc.r.Outcome, got, tc.want)
		}
	}
}
