package service

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	ksvc "github.com/kardianos/service"

	"github.com/patrick-1984/ai-minion/internal/identity"
	"github.com/patrick-1984/ai-minion/internal/worker/localapi"
)

const (
	testSID   = "S-1-5-21-1004336348-1177238915-682003330-1001"
	testState = `C:\Users\alice\AppData\Local\minion\state`
	testBin   = `C:\Program Files\minion\minion.exe`
)

func TestPipeUserSIDEnvIsTheOneTheWorkerReads(t *testing.T) {
	if pipeUserSIDEnv != localapi.PipeUserSIDEnv {
		t.Fatalf("service writes %q, the Worker reads %q", pipeUserSIDEnv, localapi.PipeUserSIDEnv)
	}
	if pipeUserSIDEnv != identity.PipeUserSIDEnv {
		t.Fatalf("service writes %q, the state protection reads %q", pipeUserSIDEnv, identity.PipeUserSIDEnv)
	}
}

// errDenied stands for ERROR_ACCESS_DENIED with a message in another language.
var errDenied = errors.New("zugriff verweigert")

// denyAccess makes the install error hint treat errDenied as access denied for
// the rest of the test; the real check is by Windows error number.
func denyAccess(t *testing.T) {
	t.Helper()
	old := accessDenied
	accessDenied = func(err error) bool { return errors.Is(err, errDenied) }
	t.Cleanup(func() { accessDenied = old })
}

// AC: the service has the same settings: it runs the entry point of the
// Windows service and reads the state directory `worker up` saved to.
func TestWindowsConfigPinsTheSettingsAndThePipeUser(t *testing.T) {
	c := WindowsConfig(testBin, testState, testSID)
	if c.Name != WindowsServiceName || c.Executable != testBin || strings.Join(c.Arguments, " ") != "worker "+WindowsServiceCommand {
		t.Errorf("config = %+v", c)
	}
	if c.EnvVars["MINION_STATE_DIR"] != testState {
		t.Errorf("MINION_STATE_DIR = %q, want %q", c.EnvVars["MINION_STATE_DIR"], testState)
	}
	if c.EnvVars[localapi.PipeUserSIDEnv] != testSID {
		t.Errorf("%s = %q, want %q", localapi.PipeUserSIDEnv, c.EnvVars[localapi.PipeUserSIDEnv], testSID)
	}
	if len(c.EnvVars) != 2 {
		t.Errorf("EnvVars = %v, want exactly the state directory and the pipe user", c.EnvVars)
	}
	// LocalSystem needs no password: no account is set.
	if c.UserName != "" {
		t.Errorf("UserName = %q", c.UserName)
	}
}

func TestWindowsConfigStartsAtBootAndRestartsOnFailure(t *testing.T) {
	o := WindowsConfig(testBin, testState, testSID).Option
	for k, want := range map[string]string{"StartType": "automatic", "OnFailure": "restart", "OnFailureDelayDuration": "5s"} {
		if got, _ := o[k].(string); got != want {
			t.Errorf("Option[%s] = %v, want %s", k, o[k], want)
		}
	}
	// kardianos/service passes the reset period to the Windows API unchanged,
	// which takes seconds: one day is 86400.
	if o["OnFailureResetPeriod"] != 86400 {
		t.Errorf("OnFailureResetPeriod = %v, want 86400 seconds", o["OnFailureResetPeriod"])
	}
}

func TestWindowsConfigLeavesOutEmptyValues(t *testing.T) {
	c := WindowsConfig(testBin, "", "")
	if len(c.EnvVars) != 0 {
		t.Errorf("EnvVars = %v", c.EnvVars)
	}
}

func TestNewConfigControllerBuilds(t *testing.T) {
	if c, err := NewConfigController(WindowsConfig(testBin, testState, testSID)); err == nil && c == nil {
		t.Fatal("nil controller without an error")
	}
}

type windowsInstallFixture struct {
	fc  *fakeController
	cfg *ksvc.Config
}

func windowsDeps(t *testing.T, fc *fakeController) (Deps, *windowsInstallFixture) {
	t.Helper()
	fx := &windowsInstallFixture{fc: fc}
	d := testDeps(t, nil)
	d.UserSID = func() (string, error) { return testSID, nil }
	d.NewControllerFromConfig = func(cfg *ksvc.Config) (Controller, error) { fx.cfg = cfg; return fc, nil }
	return d, fx
}

func TestInstallOnWindowsBuildsTheConfigFromTheRequest(t *testing.T) {
	d, fx := windowsDeps(t, &fakeController{statuses: []Status{StatusStopped, StatusRunning}})
	var out bytes.Buffer
	res, err := Install(context.Background(), d, Request{
		Manager: ManagerWindows, Scope: ScopeSystem, Binary: testBin, StateDir: testState,
	}, &out)
	if err != nil {
		t.Fatal(err)
	}
	if fx.cfg == nil || fx.cfg.EnvVars["MINION_STATE_DIR"] != testState || fx.cfg.EnvVars[localapi.PipeUserSIDEnv] != testSID {
		t.Fatalf("config = %+v", fx.cfg)
	}
	if got := strings.Join(fx.fc.calls, ","); !strings.HasPrefix(got, "uninstall,install,start,status") {
		t.Errorf("calls = %s", got)
	}
	if res.Unit != WindowsServiceName || res.Manager != ManagerWindows {
		t.Errorf("result = %+v", res)
	}
	// The operator is told what the preview service is and how to run
	// sessions as themselves.
	for _, want := range []string{"Installed windows system service MinionWorker", "Windows preview", "LocalSystem", "--foreground"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out.String())
		}
	}
}

func TestInstallOnWindowsFailsWhenTheUserSIDIsUnknown(t *testing.T) {
	d, fx := windowsDeps(t, &fakeController{})
	d.UserSID = func() (string, error) { return "", errors.New("no token") }
	_, err := Install(context.Background(), d, Request{Manager: ManagerWindows, Binary: testBin, StateDir: testState}, nil)
	if err == nil || !strings.Contains(err.Error(), "user SID") {
		t.Fatalf("err = %v", err)
	}
	if fx.cfg != nil || len(fx.fc.calls) != 0 {
		t.Errorf("a service was touched: cfg=%v calls=%v", fx.cfg, fx.fc.calls)
	}
}

func TestInstallOnWindowsReportsAControllerThatCannotBeBuilt(t *testing.T) {
	d, _ := windowsDeps(t, nil)
	d.NewControllerFromConfig = func(*ksvc.Config) (Controller, error) { return nil, errors.New("no scm") }
	if _, err := Install(context.Background(), d, Request{Manager: ManagerWindows, Binary: testBin}, nil); err == nil || !strings.Contains(err.Error(), "no scm") {
		t.Fatalf("err = %v", err)
	}
}

// The default user SID source only exists on Windows: elsewhere the install
// says so instead of installing a service for nobody.
func TestCurrentUserSIDOnThisPlatform(t *testing.T) {
	sid, err := CurrentUserSID()
	if err != nil {
		if sid != "" {
			t.Errorf("sid %q with error %v", sid, err)
		}
		return
	}
	if !localapi.ValidSID(sid) {
		t.Errorf("CurrentUserSID = %q, not a valid SID", sid)
	}
}

func TestWindowsInstallErrorExplainsAccessDenied(t *testing.T) {
	denyAccess(t)
	err := windowsInstallError("install", errDenied)
	for _, want := range []string{"install the service", "zugriff verweigert", "administrator", "--foreground"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error lacks %q: %v", want, err)
		}
	}
	plain := windowsInstallError("start", errors.New("boom"))
	if strings.Contains(plain.Error(), "administrator") || !strings.Contains(plain.Error(), "start the service: boom") {
		t.Errorf("plain error = %v", plain)
	}
}

func TestInstallOnWindowsAccessDeniedHintsAtAdministrator(t *testing.T) {
	denyAccess(t)
	d, _ := windowsDeps(t, &fakeController{installEr: errDenied})
	_, err := Install(context.Background(), d, Request{Manager: ManagerWindows, Binary: testBin}, nil)
	if err == nil || !strings.Contains(err.Error(), "administrator") {
		t.Fatalf("err = %v", err)
	}
}

// The English text is not what identifies the error: without the error number
// the hint is absent, whatever the message says.
func TestWindowsInstallErrorIgnoresTheMessageText(t *testing.T) {
	denyAccess(t)
	err := windowsInstallError("install", errors.New("Access is denied."))
	if strings.Contains(err.Error(), "administrator") {
		t.Errorf("the hint was added for a message only: %v", err)
	}
}
