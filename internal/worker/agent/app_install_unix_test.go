//go:build unix

package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/filexfer"
	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
)

// testAppInstaller returns an installer over a temporary state folder and
// Minion files folder, and the two paths.
func testAppInstaller(t *testing.T) (*appInstaller, string, string) {
	t.Helper()
	filexfer.SetDefaultResolveMode(filexfer.ResolveWalk)
	base := t.TempDir()
	state, files := filepath.Join(base, "state"), filepath.Join(base, "home", ConfineLabelAndroid)
	for _, d := range []string{state, files} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	a := &appInstaller{dir: filepath.Join(state, apkUpdateDir), confine: files,
		poll: 5 * time.Millisecond, ttl: 300 * time.Millisecond, wait: 2 * time.Second}
	return a, state, files
}

func writeAppStatus(t *testing.T, a *appInstaller, st appInstallAppState) {
	t.Helper()
	if err := os.MkdirAll(a.dir, 0o700); err != nil {
		t.Fatal(err)
	}
	st.Schema = 1
	b, _ := json.Marshal(st)
	if err := os.WriteFile(filepath.Join(a.dir, appInstallAppFile), b, 0o600); err != nil {
		t.Fatal(err)
	}
}

func appRequest(op string, args any) Request {
	raw, _ := json.Marshal(args)
	return Request{Op: op, Args: raw, Actions: []ops.Action{ops.ActionAppInstall},
		Origin: &proto.Origin{Kind: proto.OriginUser, UserID: "owner"}}
}

func callAppInstall(t *testing.T, a *appInstaller, args ops.AppInstallArgs) (ops.AppInstallResult, error) {
	t.Helper()
	res, err := a.handleInstall(context.Background(), appRequest(ops.OpAppInstall, args))
	if err != nil {
		return ops.AppInstallResult{}, err
	}
	return res.(ops.AppInstallResult), nil
}

// fakeInstallApp answers the first request it sees with state (and the facts).
func fakeInstallApp(t *testing.T, a *appInstaller, state, reason string) <-chan appInstallRequest {
	t.Helper()
	seen := make(chan appInstallRequest, 1)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() {
		for ctx.Err() == nil {
			if r, ok := a.readRequest(); ok {
				writeAppStatus(t, a, appInstallAppState{Ready: true, Attempt: &appInstallAppAttempt{ID: r.ID, State: state, Reason: reason,
					Package: "org.example.app", VersionCode: 7, VersionName: "1.7", Label: "Example",
					SignerSHA256: strings.Repeat("AB", 32), NewInstall: true}})
				seen <- r
				return
			}
			time.Sleep(2 * time.Millisecond)
		}
	}()
	return seen
}

func TestAppInstallHandsTheAPKToTheApp(t *testing.T) {
	a, _, files := testAppInstaller(t)
	writeAppStatus(t, a, appInstallAppState{Ready: true})
	body := []byte("PK\x03\x04 fake apk bytes")
	if err := os.MkdirAll(filepath.Join(files, "apps"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(files, "apps", "example.apk"), body, 0o600); err != nil {
		t.Fatal(err)
	}
	seen := fakeInstallApp(t, a, appStateInstalled, "")
	res, err := callAppInstall(t, a, ops.AppInstallArgs{Path: "apps/example.apk", AllowNew: true})
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(body)
	want := hex.EncodeToString(sum[:])
	if res.Outcome != ops.AppInstallInstalled || res.SHA256 != want || res.Size != int64(len(body)) ||
		res.Package != "org.example.app" || res.VersionCode != 7 || res.SignerSHA256 != strings.Repeat("ab", 32) {
		t.Fatalf("result = %+v", res)
	}
	r := <-seen
	if r.Path != filepath.Join(files, "apps", "example.apk") || r.RelPath != "apps/example.apk" || r.SHA256 != want ||
		!r.AllowNew || r.Size != int64(len(body)) || len(r.ID) != 24 || !r.ExpiresAt.After(r.CreatedAt) {
		t.Fatalf("request = %+v", r)
	}
	// A final outcome withdraws the request.
	if _, ok := a.readRequest(); ok {
		t.Fatal("the request is still there after a final outcome")
	}
}

func TestAppInstallOutcomes(t *testing.T) {
	for _, tc := range []struct{ state, want string }{
		{appStateConfirmation, ops.AppInstallNeedsConfirmation},
		{appStateRefused, ops.AppInstallRefused},
		{appStateDeclined, ops.AppInstallRefused},
		{appStateFailed, ops.AppInstallFailed},
	} {
		t.Run(tc.state, func(t *testing.T) {
			a, _, files := testAppInstaller(t)
			writeAppStatus(t, a, appInstallAppState{Ready: true})
			if err := os.WriteFile(filepath.Join(files, "x.apk"), []byte("apk"), 0o600); err != nil {
				t.Fatal(err)
			}
			fakeInstallApp(t, a, tc.state, "why")
			res, err := callAppInstall(t, a, ops.AppInstallArgs{Path: "x.apk"})
			if err != nil {
				t.Fatal(err)
			}
			if res.Outcome != tc.want || res.Reason == "" {
				t.Fatalf("result = %+v, want %s", res, tc.want)
			}
			_, pending := a.readRequest()
			if pending != (tc.state == appStateConfirmation) {
				t.Fatalf("request present = %v after %s", pending, tc.state)
			}
		})
	}
}

func TestAppInstallNotTakenInTime(t *testing.T) {
	a, _, files := testAppInstaller(t)
	writeAppStatus(t, a, appInstallAppState{Ready: true})
	if err := os.WriteFile(filepath.Join(files, "x.apk"), []byte("apk"), 0o600); err != nil {
		t.Fatal(err)
	}
	res, err := callAppInstall(t, a, ops.AppInstallArgs{Path: "x.apk"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != ops.AppInstallFailed || !strings.Contains(res.Reason, "did not take") {
		t.Fatalf("result = %+v", res)
	}
	if _, ok := a.readRequest(); ok {
		t.Fatal("an expired request was not withdrawn")
	}
}

func TestAppInstallConfinement(t *testing.T) {
	a, state, files := testAppInstaller(t)
	writeAppStatus(t, a, appInstallAppState{Ready: true})
	outside := filepath.Join(state, "secret.apk")
	if err := os.WriteFile(outside, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(files, "link.apk")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(state, filepath.Join(files, "dir")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(files, "folder.apk"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(files, "empty.apk"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		path string
		code proto.Code
	}{
		{"../state/secret.apk", proto.CodeInvalidArgument},
		{"/etc/x.apk", proto.CodeInvalidArgument},
		{"notes.txt", proto.CodeInvalidArgument},
		{"link.apk", proto.CodeFileTypeRefused},
		{"dir/secret.apk", proto.CodeFileTypeRefused},
		{"folder.apk", proto.CodeFileTypeRefused},
		{"missing.apk", proto.CodeNotFound},
		{"empty.apk", proto.CodeInvalidArgument},
	} {
		_, err := callAppInstall(t, a, ops.AppInstallArgs{Path: tc.path, AllowNew: true})
		ae, ok := AsAPIError(err)
		if !ok || ae.Code != tc.code {
			t.Fatalf("%s: error = %v, want %s", tc.path, err, tc.code)
		}
	}
	if _, ok := a.readRequest(); ok {
		t.Fatal("a refused path reached the app")
	}
}

func TestAppInstallSizeLimit(t *testing.T) {
	a, _, files := testAppInstaller(t)
	writeAppStatus(t, a, appInstallAppState{Ready: true})
	f, err := os.Create(filepath.Join(files, "big.apk"))
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(ops.AppInstallMaxBytes + 1); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	_, err = callAppInstall(t, a, ops.AppInstallArgs{Path: "big.apk"})
	if ae, ok := AsAPIError(err); !ok || ae.Code != proto.CodeTransferTooLarge {
		t.Fatalf("error = %v, want transfer_too_large", err)
	}
}

func TestAppInstallNeedsAReadyApp(t *testing.T) {
	a, _, files := testAppInstaller(t)
	if err := os.WriteFile(filepath.Join(files, "x.apk"), []byte("apk"), 0o600); err != nil {
		t.Fatal(err)
	}
	res, err := callAppInstall(t, a, ops.AppInstallArgs{Path: "x.apk"})
	if err != nil || res.Outcome != ops.AppInstallRefused || !strings.Contains(res.Reason, "open the app") {
		t.Fatalf("no app status: %+v, %v", res, err)
	}
	writeAppStatus(t, a, appInstallAppState{Ready: false, BlockReason: "Install unknown apps is not allowed for Minion"})
	res, err = callAppInstall(t, a, ops.AppInstallArgs{Path: "x.apk"})
	if err != nil || res.Outcome != ops.AppInstallRefused || !strings.Contains(res.Reason, "Install unknown apps") {
		t.Fatalf("blocked app: %+v, %v", res, err)
	}
}

func TestAppInstallRefusesLocalCallersAndOtherWorkers(t *testing.T) {
	a, _, _ := testAppInstaller(t)
	raw, _ := json.Marshal(ops.AppInstallArgs{Path: "x.apk"})
	_, err := a.handleInstall(context.Background(), Request{Op: ops.OpAppInstall, Args: raw, Actions: []ops.Action{ops.ActionAppInstall}})
	if ae, ok := AsAPIError(err); !ok || ae.Code != proto.CodeForbidden {
		t.Fatalf("local caller: %v", err)
	}
	other := "01890000-0000-7000-8000-000000000001"
	rq := appRequest(ops.OpAppInstall, ops.AppInstallArgs{WorkerID: other, Path: "x.apk"})
	rq.TargetWorkerID = "01890000-0000-7000-8000-000000000002"
	if _, err := a.handleInstall(context.Background(), rq); err == nil {
		t.Fatal("another Worker's id was accepted")
	}
	// operate alone is not app_install.
	rq = appRequest(ops.OpAppInstall, ops.AppInstallArgs{Path: "x.apk"})
	rq.Actions = []ops.Action{ops.ActionRead, ops.ActionOperate, ops.ActionFileWrite}
	if _, err := a.handleInstall(context.Background(), rq); err == nil {
		t.Fatal("operate and file_write allowed app_install")
	}
}

func TestAppInstallStatusListsAPKs(t *testing.T) {
	a, _, files := testAppInstaller(t)
	for _, n := range []string{"b.apk", "A.APK", ".hidden.apk", "notes.txt"} {
		if err := os.WriteFile(filepath.Join(files, n), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(files, "d.apk"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(files, "b.apk"), filepath.Join(files, "l.apk")); err != nil {
		t.Fatal(err)
	}
	writeAppStatus(t, a, appInstallAppState{Ready: true, Attempt: &appInstallAppAttempt{ID: "abc", State: appStateConfirmation, Package: "org.example.app"}})
	res, err := a.handleStatus(context.Background(), appRequest(ops.OpAppInstallStatus, ops.AppInstallStatusArgs{}))
	if err != nil {
		t.Fatal(err)
	}
	st := res.(ops.AppInstallStatus)
	if !st.Ready || st.Last == nil || st.Last.Outcome != ops.AppInstallNeedsConfirmation || st.Last.Package != "org.example.app" {
		t.Fatalf("status = %+v", st)
	}
	if len(st.Files) != 2 || st.Files[0].Path != "A.APK" || st.Files[1].Path != "b.apk" {
		t.Fatalf("files = %+v", st.Files)
	}
}

func TestAppInstallFeatureOnlyWhenAttached(t *testing.T) {
	e := &Executor{handlers: map[string]Handler{}}
	for _, f := range e.Features() {
		if f == proto.FeatureAppInstallV1 {
			t.Fatal("app.install.v1 announced without the Android app")
		}
	}
	e.AttachAppInstall(t.TempDir(), t.TempDir(), nil)
	found := false
	for _, f := range e.Features() {
		found = found || f == proto.FeatureAppInstallV1
	}
	if !found {
		t.Fatal("app.install.v1 not announced")
	}
}
